package daemon

import (
	"fmt"
	"net"
	"sync"

	"github.com/AliyunContainerService/terway/pkg/aliyun"
	"github.com/AliyunContainerService/terway/pkg/pool"
	"github.com/AliyunContainerService/terway/types"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

const (
	maxEniOperating = 3
	maxIPBacklog    = 10
)

type eniIPFactory struct {
	eniFactory   *eniFactory
	enis         []*ENI
	primaryIP    net.IP
	eniOperChan  chan struct{}
	ipResultChan chan *ENIIP
	sync.RWMutex
}

// ENIIP the secondary ip of eni
type ENIIP struct {
	*types.ENIIP
	err error
}

// ENI to hold ENI's secondary config
type ENI struct {
	lock sync.Mutex
	*types.ENI
	ips       []*ENIIP
	primaryIP net.IP
	pending   int
	ipBacklog chan struct{}
	ecs       aliyun.ECS
	done      chan struct{}
}

// eni ip allocator
func (e *ENI) allocateWorker(resultChan chan<- *ENIIP) {
	for {
		toAllocate := 0
		select {
		case <-e.done:
			return
		case <-e.ipBacklog:
			toAllocate = 1
		}

	popAll:
		for {
			select {
			case <-e.ipBacklog:
				toAllocate++
			default:
				break popAll
			}
		}
		logrus.Debugf("allocate %v ips for eni", toAllocate)
		ips, err := e.ecs.AssignNIPsForENI(e.ENI.ID, toAllocate)
		logrus.Debugf("allocated ips for eni: %v, %v, %v", e.ENI, ips, err)
		if err != nil {
			logrus.Errorf("error allocate ips for eni: %v", err)
			for i := 0; i < toAllocate; i++ {
				resultChan <- &ENIIP{
					ENIIP: &types.ENIIP{
						Eni: e.ENI,
					},
					err: errors.Errorf("error assign ip for ENI: %v", err),
				}
			}
		} else {
			for _, ip := range ips {
				resultChan <- &ENIIP{
					ENIIP: &types.ENIIP{
						Eni:        e.ENI,
						SecAddress: ip,
						PrimaryIP:  e.primaryIP,
					},
					err: nil,
				}
			}
		}
	}
}

func (f *eniIPFactory) submit() error {
	f.Lock()
	defer f.Unlock()
	for _, eni := range f.enis {
		logrus.Debugf("check exist eni's ip: %+v", eni)
		eni.lock.Lock()
		ipCount := eni.pending + len(eni.ips)
		if ipCount < eni.MaxIPs {
			select {
			case eni.ipBacklog <- struct{}{}:
			default:
				eni.lock.Unlock()
				continue
			}
			eni.pending++
			eni.lock.Unlock()
			return nil
		}
		eni.lock.Unlock()
	}
	return errors.Errorf("trigger ENIIP throttle, max operating concurrent: %v", maxIPBacklog)
}

func (f *eniIPFactory) popResult() (ip *types.ENIIP, err error) {
	result := <-f.ipResultChan
	if result.ENIIP == nil || result.err != nil {
		f.Lock()
		defer f.Unlock()
		if result.Eni != nil {
			for _, eni := range f.enis {
				if eni.MAC == result.Eni.MAC {
					eni.pending--
				}
			}
		}
		return nil, errors.Errorf("error allocate ip from eni: %v", result.err)
	}
	f.Lock()
	defer f.Unlock()
	for _, eni := range f.enis {
		if eni.MAC == result.Eni.MAC {
			eni.pending--
			eni.lock.Lock()
			eni.ips = append(eni.ips, result)
			eni.lock.Unlock()
			return result.ENIIP, nil
		}
	}
	return nil, errors.Errorf("unexpected eni ip allocated: %v", result)

}

func (f *eniIPFactory) Create() (types.NetworkResource, error) {
	var (
		ip  *types.ENIIP
		err error
	)
	defer func() {
		if ip == nil {
			logrus.Debugf("create result: %v, error: %v", ip, err)
		} else {
			logrus.Debugf("create result nil: %v, error: %v", ip.GetResourceID(), err)
		}
	}()

	err = f.submit()
	if err == nil {
		ip, err = f.popResult()
		return ip, err
	}
	logrus.Debugf("allocate from exist eni error: %v, creating eni", err)

	select {
	case f.eniOperChan <- struct{}{}:
	default:
		return nil, errors.Errorf("trigger ENI throttle, max operating concurrent: %v", maxEniOperating)
	}
	rawEni, err := f.eniFactory.Create()
	<-f.eniOperChan
	if err != nil {
		return nil, err
	}

	eniObj, ok := rawEni.(*types.ENI)
	if !ok {
		return nil, errors.Errorf("error get type ENI from factory, got: %v", rawEni)
	}

	eni := &ENI{
		ENI:       eniObj,
		ecs:       f.eniFactory.ecs,
		primaryIP: f.primaryIP,
		ipBacklog: make(chan struct{}, maxIPBacklog),
		done:      make(chan struct{}, 1),
	}

	mainENIIP := &types.ENIIP{
		Eni:        eni.ENI,
		SecAddress: eni.ENI.Address.IP,
		PrimaryIP:  eni.primaryIP,
	}

	eni.ips = append(eni.ips, &ENIIP{
		ENIIP: mainENIIP,
	})

	f.Lock()
	f.enis = append(f.enis, eni)
	go eni.allocateWorker(f.ipResultChan)
	f.Unlock()

	return mainENIIP, nil
}

func (f *eniIPFactory) Dispose(res types.NetworkResource) (err error) {
	defer func() {
		logrus.Debugf("dispose result: %v, error: %v", res.GetResourceID(), err != nil)
	}()
	ip := res.(*types.ENIIP)
	var (
		eni   *ENI
		eniip *ENIIP
	)
	f.RLock()
	for _, e := range f.enis {
		if ip.Eni.ID == e.ID {
			eni = e
			e.lock.Lock()
			for _, eip := range e.ips {
				if eip.SecAddress.String() == ip.SecAddress.String() {
					eniip = eip
				}
			}
			e.lock.Unlock()
		}
	}
	f.RUnlock()
	if eni == nil || eniip == nil {
		return fmt.Errorf("invalid resource to dispose")
	}

	eni.lock.Lock()
	if len(eni.ips) == 1 {
		if eni.pending > 0 {
			eni.lock.Unlock()
			return fmt.Errorf("ENI have pending ips to be allocate")
		}
		// block ip allocate
		eni.pending = eni.MaxIPs
		eni.lock.Unlock()

		f.Lock()
		for i, e := range f.enis {
			if ip.Eni.ID == e.ID {
				close(eni.done)
				f.enis[len(f.enis)-1], f.enis[i] = f.enis[i], f.enis[len(f.enis)-1]
				f.enis = f.enis[:len(f.enis)-1]
				break
			}
		}
		f.Unlock()

		f.eniOperChan <- struct{}{}
		// only remain ENI main ip address, release the ENI interface
		err = f.eniFactory.Dispose(ip.Eni)
		<-f.eniOperChan
		if err != nil {
			return fmt.Errorf("error dispose ENI for eniip, %v", err)
		}
		return nil
	}
	eni.lock.Unlock()

	// main ip of ENI, raise put_it_back error
	if ip.Eni.Address.IP.Equal(ip.SecAddress) {
		return fmt.Errorf("ip to be release is primary ip of ENI")
	}

	err = f.eniFactory.ecs.UnAssignIPForENI(ip.Eni.ID, ip.SecAddress)
	if err != nil {
		return fmt.Errorf("error unassign eniip, %v", err)
	}
	eni.lock.Lock()
	for i, e := range eni.ips {
		if e.SecAddress.Equal(eniip.SecAddress) {
			eni.ips[len(eni.ips)-1], eni.ips[i] = eni.ips[i], eni.ips[len(eni.ips)-1]
			eni.ips = eni.ips[:len(eni.ips)-1]
			break
		}
	}
	eni.lock.Unlock()
	return nil
}

type eniIPResourceManager struct {
	pool pool.ObjectPool
}

func newENIIPResourceManager(poolConfig *types.PoolConfig, ecs aliyun.ECS, allocatedResources []resourceManagerInitItem) (ResourceManager, error) {
	primaryIP, err := aliyun.GetPrivateIPV4()
	if err != nil {
		return nil, errors.Wrapf(err, "get primary ip error")
	}
	logrus.Infof("node's primary ip is %v", primaryIP)

	eniFactory, err := newENIFactory(poolConfig, ecs)
	if err != nil {
		return nil, errors.Wrapf(err, "error get ENI factory for eniip factory")
	}

	factory := &eniIPFactory{
		eniFactory:   eniFactory,
		enis:         []*ENI{},
		primaryIP:    primaryIP,
		eniOperChan:  make(chan struct{}, maxEniOperating),
		ipResultChan: make(chan *ENIIP, maxIPBacklog),
	}

	capacity, err := ecs.GetInstanceMaxPrivateIP(poolConfig.InstanceID)
	if err != nil {
		return nil, errors.Wrapf(err, "error get eniip max capacity for eniip factory")
	}

	if poolConfig.MaxPoolSize > capacity {
		logrus.Infof("max pool size bigger than node capacity, set max pool size to capacity")
		poolConfig.MaxPoolSize = capacity
	}

	poolCfg := pool.Config{
		MaxIdle:  poolConfig.MaxPoolSize,
		MinIdle:  poolConfig.MinPoolSize,
		Factory:  factory,
		Capacity: capacity,
		Initializer: func(holder pool.ResourceHolder) error {
			// not use main ENI for ENI multiple ip allocate
			enis, err := ecs.GetAttachedENIs(poolConfig.InstanceID, false)
			if err != nil {
				return errors.Wrapf(err, "error get attach ENI on pool init")
			}
			stubMap := make(map[string]*podInfo)
			for _, allocated := range allocatedResources {
				stubMap[allocated.resourceID] = allocated.podInfo
			}

			for _, eni := range enis {
				ips, err := ecs.GetENIIPs(eni.ID)
				if err != nil {
					return errors.Wrapf(err, "error get ENI's ip on pool init")
				}
				poolENI := &ENI{
					lock:      sync.Mutex{},
					ENI:       eni,
					ips:       []*ENIIP{},
					ecs:       ecs,
					primaryIP: primaryIP,
					ipBacklog: make(chan struct{}, maxIPBacklog),
					done:      make(chan struct{}, 1),
				}
				factory.enis = append(factory.enis, poolENI)
				for _, ip := range ips {
					eniIP := &types.ENIIP{
						Eni:        eni,
						SecAddress: ip,
						PrimaryIP:  primaryIP,
					}
					podInfo, ok := stubMap[eniIP.GetResourceID()]

					poolENI.ips = append(poolENI.ips, &ENIIP{
						ENIIP: eniIP,
					})
					if !ok {
						holder.AddIdle(eniIP)
					} else {
						holder.AddInuse(eniIP, podInfoKey(podInfo.Namespace, podInfo.Name))
					}
				}
				logrus.Debugf("init factory's exist ENI: %+v", poolENI)
				go poolENI.allocateWorker(factory.ipResultChan)
			}
			return nil
		},
	}
	pool, err := pool.NewSimpleObjectPool(poolCfg)
	if err != nil {
		return nil, err
	}
	return &eniIPResourceManager{
		pool: pool,
	}, nil
}

func (m *eniIPResourceManager) Allocate(ctx *networkContext, prefer string) (types.NetworkResource, error) {
	return m.pool.Acquire(ctx, prefer, podInfoKey(ctx.pod.Namespace, ctx.pod.Name))
}

func (m *eniIPResourceManager) Release(context *networkContext, resID string) error {
	if context != nil && context.pod != nil {
		return m.pool.ReleaseWithReverse(resID, context.pod.IPStickTime)
	}
	return m.pool.Release(resID)
}

func (m *eniIPResourceManager) GarbageCollection(inUseSet map[string]interface{}, expireResSet map[string]interface{}) error {
	for expireRes := range expireResSet {
		if err := m.pool.Stat(expireRes); err == nil {
			err = m.Release(nil, expireRes)
			if err != nil {
				return err
			}
		}
	}
	return nil
}
