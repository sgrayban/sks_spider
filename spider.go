/*
   Copyright 2009-2013 Phil Pennock

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package sks_spider

// We query a host as few times as possible, capturing the hostnames
// under which it's known and the aliases, and de-duping by IP address

import (
	"fmt"
	"net"
	"strings"
	"sync"
)

const QUEUE_DEPTH int = 100

type DnsResult struct {
	hostname string
	ipList   []string
	err      error
}

type HostsRequest struct {
	hostnames []string
	distance  int
	origin    string
}

type HostResult struct {
	hostname string
	node     *SksNode
	err      error
}

type CountryResult struct {
	ip      string
	country string
	err     error
}

type spiderShared struct {
	dnsResult     chan *DnsResult
	hostResult    chan *HostResult
	countryResult chan *CountryResult
}

// This persists for the length of one data gathering run.
type Spider struct {
	batchAddHost     chan *HostsRequest
	pending          sync.WaitGroup
	shared           *spiderShared
	considering      map[string]bool     // already looking this host up in DNS
	badDNS           map[string]bool     // record bogus hostnames
	knownHosts       map[string]string   // aliases to canonical hostname from server info page
	aliasesForHost   map[string][]string // for a hostname, reverse aliases
	knownIPs         map[string]string   // IPs to same canonical hostname
	ipsForHost       map[string][]string // for a given DNS lookup, the IP results
	serverInfos      map[string]*SksNode // key should be canonical hostname
	queryErrors      map[string]error
	pendingHosts     map[string]int // diagnostics when "hung"
	pendingCountries map[string]int
	distances        map[string]int
	countriesForIPs  map[string]string
	terminate        chan bool
}

func StartSpider() *Spider {
	shared := new(spiderShared)
	shared.dnsResult = make(chan *DnsResult, QUEUE_DEPTH)
	shared.hostResult = make(chan *HostResult, QUEUE_DEPTH)
	shared.countryResult = make(chan *CountryResult, QUEUE_DEPTH)

	spider := new(Spider)
	spider.shared = shared
	spider.batchAddHost = make(chan *HostsRequest, QUEUE_DEPTH)
	spider.considering = make(map[string]bool)
	spider.badDNS = make(map[string]bool)
	spider.knownHosts = make(map[string]string)
	spider.aliasesForHost = make(map[string][]string)
	spider.knownIPs = make(map[string]string)
	spider.ipsForHost = make(map[string][]string)
	spider.serverInfos = make(map[string]*SksNode)
	spider.queryErrors = make(map[string]error)
	spider.pendingHosts = make(map[string]int)
	spider.pendingCountries = make(map[string]int)
	spider.distances = make(map[string]int)
	spider.countriesForIPs = make(map[string]string)
	spider.terminate = make(chan bool)

	KillDummySpiderForDiagnosticsChannel()
	go spiderMainLoop(spider)
	return spider
}

func (spider *Spider) Wait() {
	// AddHost bumps counter in context of caller, so should call initial AddHost
	// and ensure that your Wait comes after that.
	// MUST ensure that spawn other go-routines with a .Add first, before the caller's
	// count is decremented.

	// SHOULD only call .Done() from the spiderMainLoop()

	// While under initial Add, we might start a DNS lookup; if we don't, then the
	// code which decides against is responsible for also dropping counter.
	// If we do, then it's only once the DNS result has been processed and we're back
	// in spiderMainLoop() that we drop that counter.
	// Before finishing the DNS result, we might spawn a host query, bumping count.
	// When the host query has finished and we're back in spiderMainLoop(), we drop
	// again.  While still processing those results, before returning, we might inline
	// batch-add more hosts.

	// SO: should be no need to also check channel lengths and risk races.
	spider.pending.Wait()
}

func (spider *Spider) Terminate() {
	spider.terminate <- true
	go DummySpiderForDiagnosticsChannel()
}

func (spider *Spider) AddHost(hostname string, distance int) {
	spider.pending.Add(1)
	spider.pendingHosts[hostname] += 1
	spider.batchAddHost <- &HostsRequest{hostnames: []string{hostname}, distance: distance}
}

func (spider *Spider) BatchAddHost(origin string, hostlist []string) {
	spider.pending.Add(len(hostlist))
	for _, h := range hostlist {
		spider.pendingHosts[h] += 1
	}
	spider.batchAddHost <- &HostsRequest{hostnames: hostlist, origin: origin}
}

func spiderMainLoop(spider *Spider) {
	for {
		select {
		case hostreq := <-spider.batchAddHost:
			for _, hostname := range hostreq.hostnames {
				spider.considerHost(hostname, hostreq)
			}
		case dnsResult := <-spider.shared.dnsResult:
			spider.processDnsResult(dnsResult)
			spider.pendingHosts[dnsResult.hostname] -= 1
			spider.pending.Done()
		case hostResult := <-spider.shared.hostResult:
			spider.processHostResult(hostResult)
			spider.pendingHosts[hostResult.hostname] -= 1
			spider.pending.Done()
		case countryResult := <-spider.shared.countryResult:
			spider.processCountryResult(countryResult)
			spider.pendingCountries[countryResult.ip] -= 1
			spider.pending.Done()
		case out := <-diagnosticSpiderDump:
			spider.diagnosticDumpInRoutine(out)
			diagnosticSpiderDone <- true
		case <-spider.terminate:
			break
		}
	}
}

func (spider *Spider) considerHost(hostname string, request *HostsRequest) {
	skip := false
	distance := -1

	if request.origin != "" {
		if d, ok := spider.distances[request.origin]; ok {
			distance = d + 1
		}
	}
	if distance < 0 {
		distance = request.distance
	}
	if olddistance, ok := spider.distances[hostname]; ok && olddistance > distance {
		Log.Printf("Promoting host to be nearer; \"%s\" was %d, now %d", hostname, olddistance, distance)
		spider.distances[hostname] = distance
	}

	if _, ok := spider.considering[hostname]; ok {
		skip = true
	} else if _, ok := BlacklistedHosts[hostname]; ok {
		Log.Printf("Ignoring blacklisted host: \"%s\"", hostname)
		skip = true
	} else if _, ok := spider.badDNS[hostname]; ok {
		skip = true
	} else if _, ok := spider.knownHosts[hostname]; ok {
		skip = true
	} else if ip := net.ParseIP(hostname); ip != nil {
		Log.Printf("Ignoring IP address: [%s]", hostname)
		skip = true
	} else if !strings.Contains(hostname, ".") {
		Log.Printf("Ignoring unqualified hostname: %s", hostname)
		skip = true
	} else if strings.Contains(hostname, "pool.") {
		Log.Printf("Ignoring pool hostname: %s", hostname)
		skip = true
	} else if strings.HasSuffix(hostname, ".local") {
		Log.Printf("Ignoring .local hostname: %s", hostname)
		skip = true
	} else {
		for _, hn := range blacklistedQueryHosts {
			if hn != hostname {
				continue
			}
			Log.Printf("Ignoring blacklisted hostname: %s", hostname)
			skip = true
		}
	}
	if skip {
		spider.pendingHosts[hostname] -= 1
		spider.pending.Done()
		return
	}

	spider.considering[hostname] = true
	spider.distances[hostname] = distance

	go func(shared *spiderShared) {
		ipList, err := net.LookupHost(hostname)
		shared.dnsResult <- &DnsResult{hostname, ipList, err}
	}(spider.shared)
}

func flattenIPs(ipLists ...[]string) []string {
	var maxlen = 0
	for i := range ipLists {
		maxlen += len(ipLists[i])
	}
	result := make([]string, 0, maxlen)
	for i := range ipLists {
		for _, ip := range ipLists[i] {
			found := false
			for _, ip2 := range result {
				if ip == ip2 {
					found = true
					break
				}
			}
			if !found {
				result = append(result, ip)
			}
		}
	}
	return result
}

func (spider *Spider) processDnsResult(dns *DnsResult) {
	hostname := dns.hostname
	if dns.err != nil {
		Log.Printf("DNS resolution failure for \"%s\": %s", hostname, dns.err)
		spider.badDNS[hostname] = true
		return
	}
	ipList := flattenIPs(dns.ipList)
	for _, ip := range ipList {
		if IPDisallowed(ip) {
			Log.Printf("Disallowing host \"%s\" because of IP [%s]", hostname, ip)
			spider.badDNS[hostname] = true
			return
		}
		canonical, ok := spider.knownIPs[ip]
		if !ok {
			continue
		}
		spider.knownHosts[hostname] = canonical
		for _, ip2 := range ipList {
			spider.knownIPs[ip2] = canonical
		}
		spider.ipsForHost[canonical] = flattenIPs(spider.ipsForHost[canonical], ipList)
		return
	}
	// should be shiny new host after this point
	spider.knownHosts[hostname] = hostname
	spider.aliasesForHost[hostname] = []string{hostname}
	spider.ipsForHost[hostname] = ipList
	for _, ip := range ipList {
		spider.knownIPs[ip] = hostname
		if _, ok2 := spider.countriesForIPs[ip]; !ok2 {
			spider.countriesForIPs[ip] = ""
			spider.pendingCountries[ip] += 1
			spider.pending.Add(1)
			go spider.shared.QueryCountryForIP(ip)
		}
	}
	spider.serverInfos[hostname] = nil
	spider.pending.Add(1)
	spider.pendingHosts[hostname] += 1
	go spider.shared.QueryHost(hostname)
}

func (sResults *spiderShared) QueryHost(hostname string) {
	node := &SksNode{Hostname: hostname}
	err := node.Fetch()
	if err != nil {
		sResults.hostResult <- &HostResult{hostname: hostname, err: err}
		return
	}
	var analyzePaniced bool = false
	func() {
		defer func() {
			if x := recover(); x != nil {
				e := fmt.Errorf("analyze panic: %v", x)
				node.analyzeError = e
				sResults.hostResult <- &HostResult{hostname: hostname, node: node, err: e}
				analyzePaniced = true
			}
		}()
		node.Analyze()
	}()
	if !analyzePaniced {
		sResults.hostResult <- &HostResult{hostname: hostname, node: node}
	}
	return
}

func (spider *Spider) processHostResult(hr *HostResult) {
	hostname := hr.hostname
	canonical := hostname
	node := hr.node
	err := hr.err
	if err != nil {
		Log.Printf("Failure fetching \"%s\": %s", hostname, err)
		spider.queryErrors[hostname] = err
		return
	}
	own_hostname, ok := node.Settings["Hostname"]

	if ok && own_hostname != hostname {
		canonical = own_hostname
		oldnode, ok2 := spider.serverInfos[canonical]
		if ok2 && oldnode != nil {
			Log.Printf("Duplicate fetch, got serverInfo for \"%s\" and again as \"%s\"", canonical, hostname)
		}

		delete(spider.serverInfos, hostname)

		if _, ok3 := spider.knownHosts[canonical]; !ok3 {
			spider.knownHosts[canonical] = canonical
		}
		for _, alias := range spider.aliasesForHost[hostname] {
			spider.knownHosts[alias] = canonical
		}
		spider.aliasesForHost[canonical] = append(spider.aliasesForHost[hostname], canonical)

		for _, ip := range spider.ipsForHost[hostname] {
			spider.knownIPs[ip] = canonical
		}
		if _, ok3 := spider.ipsForHost[canonical]; !ok3 {
			spider.ipsForHost[canonical] = spider.ipsForHost[hostname]
			delete(spider.ipsForHost, hostname)
		} else {
			spider.ipsForHost[canonical] = flattenIPs(spider.ipsForHost[canonical], spider.ipsForHost[hostname])
		}
		delete(spider.aliasesForHost, hostname)
		if old, ok3 := spider.distances[canonical]; !ok3 || old < spider.distances[hostname] {
			spider.distances[canonical] = spider.distances[hostname]
		}
	}

	own_nodename, ok := node.Settings["Nodename"]
	if ok && own_nodename != canonical && own_nodename != own_hostname {
		if _, ok2 := spider.knownHosts[own_nodename]; !ok2 {
			spider.knownHosts[own_nodename] = canonical
		}
	}

	spider.serverInfos[canonical] = node
	spider.BatchAddHost(canonical, node.GossipPeerList)
	return
}

func (sResults *spiderShared) QueryCountryForIP(ipstr string) {
	country, err := CountryForIPString(ipstr)
	sResults.countryResult <- &CountryResult{ip: ipstr, country: country, err: err}
}

func (spider *Spider) processCountryResult(cr *CountryResult) {
	if cr.err == nil {
		spider.countriesForIPs[cr.ip] = cr.country
	}
}
