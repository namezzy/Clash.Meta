package outboundgroup

import (
	"context"
	"fmt"
	"github.com/Dreamacro/clash/adapter/outbound"
	C "github.com/Dreamacro/clash/constant"
	"github.com/Dreamacro/clash/constant/provider"
	types "github.com/Dreamacro/clash/constant/provider"
	"github.com/Dreamacro/clash/log"
	"github.com/Dreamacro/clash/tunnel"
	"github.com/dlclark/regexp2"
	"go.uber.org/atomic"
	"strings"
	"sync"
	"time"
)

type GroupBase struct {
	*outbound.Base
	filterRegs    []*regexp2.Regexp
	providers     []provider.ProxyProvider
	failedTestMux sync.Mutex
	failedTimes   int
	failedTime    time.Time
	failedTesting *atomic.Bool
	proxies       [][]C.Proxy
	versions      []atomic.Uint32
}

type GroupBaseOption struct {
	outbound.BaseOption
	filter    string
	providers []provider.ProxyProvider
}

func NewGroupBase(opt GroupBaseOption) *GroupBase {
	var filterRegs []*regexp2.Regexp
	if opt.filter != "" {
		for _, filter := range strings.Split(opt.filter, "`") {
			filterReg := regexp2.MustCompile(filter, 0)
			filterRegs = append(filterRegs, filterReg)
		}
	}

	gb := &GroupBase{
		Base:          outbound.NewBase(opt.BaseOption),
		filterRegs:    filterRegs,
		providers:     opt.providers,
		failedTesting: atomic.NewBool(false),
	}

	gb.proxies = make([][]C.Proxy, len(opt.providers))
	gb.versions = make([]atomic.Uint32, len(opt.providers))

	return gb
}

func (gb *GroupBase) Touch() {
	for _, pd := range gb.providers {
		pd.Touch()
	}
}

func (gb *GroupBase) GetProxies(touch bool) []C.Proxy {
	if len(gb.filterRegs) == 0 {
		var proxies []C.Proxy
		for _, pd := range gb.providers {
			if touch {
				pd.Touch()
			}
			proxies = append(proxies, pd.Proxies()...)
		}
		if len(proxies) == 0 {
			return append(proxies, tunnel.Proxies()["COMPATIBLE"])
		}
		return proxies
	}

	for i, pd := range gb.providers {
		if touch {
			pd.Touch()
		}

		if pd.VehicleType() == types.Compatible {
			gb.versions[i].Store(pd.Version())
			gb.proxies[i] = pd.Proxies()
			continue
		}

		version := gb.versions[i].Load()
		if version != pd.Version() && gb.versions[i].CompareAndSwap(version, pd.Version()) {
			var (
				proxies    []C.Proxy
				newProxies []C.Proxy
			)

			proxies = pd.Proxies()
			proxiesSet := map[string]struct{}{}
			for _, filterReg := range gb.filterRegs {
				for _, p := range proxies {
					name := p.Name()
					if mat, _ := filterReg.FindStringMatch(name); mat != nil {
						if _, ok := proxiesSet[name]; !ok {
							proxiesSet[name] = struct{}{}
							newProxies = append(newProxies, p)
						}
					}
				}
			}

			gb.proxies[i] = newProxies
		}
	}

	var proxies []C.Proxy
	for _, p := range gb.proxies {
		proxies = append(proxies, p...)
	}

	if len(proxies) == 0 {
		return append(proxies, tunnel.Proxies()["COMPATIBLE"])
	}

	if len(gb.providers) > 1 && len(gb.filterRegs) > 1 {
		var newProxies []C.Proxy
		proxiesSet := map[string]struct{}{}
		for _, filterReg := range gb.filterRegs {
			for _, p := range proxies {
				name := p.Name()
				if mat, _ := filterReg.FindStringMatch(name); mat != nil {
					if _, ok := proxiesSet[name]; !ok {
						proxiesSet[name] = struct{}{}
						newProxies = append(newProxies, p)
					}
				}
			}
		}
		for _, p := range proxies { // add not matched proxies at the end
			name := p.Name()
			if _, ok := proxiesSet[name]; !ok {
				proxiesSet[name] = struct{}{}
				newProxies = append(newProxies, p)
			}
		}
		proxies = newProxies
	}

	return proxies
}

func (gb *GroupBase) URLTest(ctx context.Context, url string) (map[string]uint16, error) {
	var wg sync.WaitGroup
	var lock sync.Mutex
	mp := map[string]uint16{}
	proxies := gb.GetProxies(false)
	for _, proxy := range proxies {
		proxy := proxy
		wg.Add(1)
		go func() {
			delay, err := proxy.URLTest(ctx, url)
			if err == nil {
				lock.Lock()
				mp[proxy.Name()] = delay
				lock.Unlock()
			}

			wg.Done()
		}()
	}
	wg.Wait()

	if len(mp) == 0 {
		return mp, fmt.Errorf("get delay: all proxies timeout")
	} else {
		return mp, nil
	}
}

func (gb *GroupBase) onDialFailed(adapterType C.AdapterType, err error) {
	if adapterType == C.Direct || adapterType == C.Compatible || adapterType == C.Reject || adapterType == C.Pass {
		return
	}

	if strings.Contains(err.Error(), "connection refused") {
		go gb.healthCheck()
		return
	}

	go func() {
		gb.failedTestMux.Lock()
		defer gb.failedTestMux.Unlock()

		gb.failedTimes++
		if gb.failedTimes == 1 {
			log.Debugln("ProxyGroup: %s first failed", gb.Name())
			gb.failedTime = time.Now()
		} else {
			if time.Since(gb.failedTime) > gb.failedTimeoutInterval() {
				gb.failedTimes = 0
				return
			}

			log.Debugln("ProxyGroup: %s failed count: %d", gb.Name(), gb.failedTimes)
			if gb.failedTimes >= gb.maxFailedTimes() {
				log.Warnln("because %s failed multiple times, active health check", gb.Name())
				gb.healthCheck()
			}
		}
	}()
}

func (gb *GroupBase) healthCheck() {
	if gb.failedTesting.Load() {
		return
	}

	gb.failedTesting.Store(true)
	wg := sync.WaitGroup{}
	for _, proxyProvider := range gb.providers {
		wg.Add(1)
		proxyProvider := proxyProvider
		go func() {
			defer wg.Done()
			proxyProvider.HealthCheck()
		}()
	}

	wg.Wait()
	gb.failedTesting.Store(false)
	gb.failedTimes = 0
}

func (gb *GroupBase) failedIntervalTime() int64 {
	return 5 * time.Second.Milliseconds()
}

func (gb *GroupBase) onDialSuccess() {
	if !gb.failedTesting.Load() {
		gb.failedTimes = 0
	}
}

func (gb *GroupBase) maxFailedTimes() int {
	return 5
}

func (gb *GroupBase) failedTimeoutInterval() time.Duration {
	return 5 * time.Second
}
