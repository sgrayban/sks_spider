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

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

import (
	btree "github.com/runningwild/go-btree"
)

func apiIpValidPage(w http.ResponseWriter, req *http.Request) {
	var err error
	if err = req.ParseForm(); err != nil {
		http.Error(w, "Failed to parse form information", http.StatusBadRequest)
		return
	}
	var (
		showStats        bool
		emitJson         bool
		limitToProxies   bool
		limitToCountries *CountrySet
	)
	if _, ok := req.Form["stats"]; ok {
		showStats = true
	}
	if _, ok := req.Form["json"]; ok {
		emitJson = true
	}
	if _, ok := req.Form["proxies"]; ok {
		limitToProxies = true
	}
	if _, ok := req.Form["countries"]; ok {
		limitToCountries = NewCountrySet(req.Form.Get("countries"))
	}

	statsList := make([]string, 0, 100)
	Statsf := func(s string, v ...interface{}) {
		statsList = append(statsList, fmt.Sprintf(s, v...))
	}

	var (
		abortMessage func(string)
		doShowStats  func()
		contentType  string
	)

	if emitJson {
		contentType = ContentTypeJson
		if _, ok := req.Form["textplain"]; ok {
			contentType = ContentTypeTextPlain
		}
		doShowStats = func() {
			b, err := json.Marshal(statsList)
			if err != nil {
				Log.Printf("Unable to JSON marshal stats: %s", err)
				return
			}
			fmt.Fprintf(w, "\"stats\": %s\n", b)
		}
		abortMessage = func(s string) {
			fmt.Fprintf(w, "{\n")
			if showStats {
				doShowStats()
				fmt.Fprintf(w, ", ")
			}
			fmt.Fprintf(w, `"status": { "status": "INVALID", "count": 0, "reason": "%s" }`, s)
			fmt.Fprintf(w, "\n}\n")
		}
	} else {
		contentType = ContentTypeTextPlain
		doShowStats = func() {
			for _, l := range statsList {
				fmt.Fprintf(w, "STATS: %s\n", l)
			}
		}
		abortMessage = func(s string) {
			if showStats {
				doShowStats()
			}
			fmt.Fprintf(w, "IP-Gen/1.1: status=INVALID count=0 reason=%s\n.\n", s)
		}
	}
	w.Header().Set("Content-Type", contentType)

	persisted := GetCurrentPersisted()
	if persisted == nil {
		abortMessage("first_scan")
		return
	}

	var minimumVersion *SksVersion = nil
	mvReq := req.Form.Get("minimum_version")
	if mvReq != "" {
		tmp := NewSksVersion(mvReq)
		minimumVersion = tmp
	}

	var (
		// for stats, we avoid double-weighting dual-stack boxes by working with
		// just one IP per box, but then later deal with all the IPs for filtering.
		ips_one_per_server = make(map[string]int, len(persisted.HostMap)*2)
		ips_all            = make(map[string]int, len(persisted.HostMap)*2)
	)

	var (
		count_servers_1010            int
		count_servers_too_old         int
		count_servers_unwanted_server int
		count_servers_wrong_country   int
		ips_skip_1010                 btree.SortedSet = btree.NewTree(btreeStringLess)
		ips_too_old                   btree.SortedSet = btree.NewTree(btreeStringLess)
		ips_unwanted_server           btree.SortedSet = btree.NewTree(btreeStringLess)
		ips_wrong_country             btree.SortedSet = btree.NewTree(btreeStringLess)
	)

	for _, name := range persisted.Sorted {
		node := persisted.HostMap[name]
		var (
			skip_this_1010     = false
			skip_this_age      = false
			skip_this_nonproxy = false
			skip_this_country  = false
		)
		if node.Keycount <= 1 {
			Statsf("dropping server <%s> with %d keys", name, node.Keycount)
			continue
		}

		if string(node.Version) == "1.0.10" {
			skip_this_1010 = true
			//ips_skip_1010.Insert(name) // nope, IPs
			count_servers_1010 += 1
		}

		if minimumVersion != nil {
			thisVersion := NewSksVersion(node.Version)
			if thisVersion == nil || !thisVersion.IsAtLeast(minimumVersion) {
				skip_this_age = true
				count_servers_too_old += 1
			}
		}

		if limitToProxies && node.ViaHeader == "" {
			server := strings.ToLower(strings.SplitN(node.ServerHeader, "/", 2)[0])
			if _, ok := serverHeadersNative[server]; ok {
				skip_this_nonproxy = true
				count_servers_unwanted_server += 1
			}
		}

		if limitToCountries != nil {
			var keep bool
			for _, ip := range node.IpList {
				geo, ok := persisted.IPCountryMap[ip]
				if ok && limitToCountries.HasCountry(geo) {
					keep = true
				}
			}
			if !keep {
				skip_this_country = true
				count_servers_wrong_country += 1
			}
		}

		if len(node.IpList) > 0 {
			ips_one_per_server[node.IpList[0]] = node.Keycount
			for _, ip := range node.IpList {
				ips_all[ip] = node.Keycount
				if skip_this_1010 {
					ips_skip_1010.Insert(ip)
				}
				if skip_this_age {
					ips_too_old.Insert(ip)
				}
				if skip_this_nonproxy {
					ips_unwanted_server.Insert(ip)
				}
				if skip_this_country {
					ips_wrong_country.Insert(ip)
				}
			}
		}

	}

	// We want to discard statistic-distorting outliers, then of what remains,
	// discard those too far away from "normal", but we really want the "best"
	// servers to be our guide, so 1 std-dev of the second-highest remaining
	// value should be safe; in fact, we'll hardcode a limit of how far below.
	// To discard, find mode size (knowing that value can be split across two
	// buckets) and discard more than five stddevs from mode.  The bucketing
	// should be larger than the distance from desired value so that the mode
	// is only split across two buckets, if we assume enough servers that a
	// small number will be down, most will be valid-if-large-enough, so that
	// splitting the count across two buckets won't let the third-best value win

	// This is barely-modified from Python, just enough to translate language, not idioms
	// This was ... "much easier" with list comprehensions in Python
	var buckets = make(map[int][]int, 40)
	for _, count := range ips_one_per_server {
		bucket := int(count / kBUCKET_SIZE)
		if _, ok := buckets[bucket]; !ok {
			buckets[bucket] = make([]int, 0, 20)
		}
		buckets[bucket] = append(buckets[bucket], count)
	}
	if len(buckets) == 0 {
		abortMessage("broken_no_buckets")
		return
	}

	var largest_bucket int
	var largest_bucket_len int
	for k := range buckets {
		if len(buckets[k]) > largest_bucket_len {
			largest_bucket = k
			largest_bucket_len = len(buckets[k])
		}
	}
	first_n := len(buckets[largest_bucket])
	var first_sum int
	for _, v := range buckets[largest_bucket] {
		first_sum += v
	}
	first_mean := float64(first_sum) / float64(first_n)
	var first_sd float64
	for _, v := range buckets[largest_bucket] {
		d := float64(v) - first_mean
		first_sd += d * d
	}
	first_sd = math.Sqrt(first_sd / float64(first_n))
	first_bounds_min := int(first_mean - 5*first_sd)
	first_bounds_max := int(first_mean + 5*first_sd)

	first_ips_list := make([]string, 0, len(ips_one_per_server))
	for ip := range ips_one_per_server {
		if first_bounds_min <= ips_all[ip] && ips_all[ip] <= first_bounds_max {
			first_ips_list = append(first_ips_list, ip)
		}
	}
	first_ips_alllist := make([]string, 0, len(ips_all))
	for ip := range ips_all {
		if first_bounds_min <= ips_all[ip] && ips_all[ip] <= first_bounds_max {
			first_ips_alllist = append(first_ips_alllist, ip)
		}
	}
	var second_mean, second_sd float64
	first_ips := make(map[string]int, len(first_ips_list))
	for _, ip := range first_ips_list {
		first_ips[ip] = ips_all[ip]
		second_mean += float64(ips_all[ip])
	}
	first_ips_all := make(map[string]int, len(first_ips_alllist))
	for _, ip := range first_ips_alllist {
		first_ips_all[ip] = ips_all[ip]
	}
	second_mean /= float64(len(first_ips_list))
	for _, v := range first_ips {
		d := float64(v) - second_mean
		second_sd += d * d
	}
	second_sd = math.Sqrt(second_sd / float64(len(first_ips_list)))

	if showStats {
		Statsf("have %d servers in %d buckets (%d ips total)", len(ips_one_per_server), len(buckets), len(ips_all))
		bucket_sizes := make([]int, 0, len(buckets))
		for k := range buckets {
			bucket_sizes = append(bucket_sizes, k)
		}
		sort.Ints(bucket_sizes)
		for _, b := range bucket_sizes {
			Statsf("%6d: %s", b, strings.Repeat("*", len(buckets[b])))
		}
		Statsf("largest bucket is %d with %d entries", largest_bucket, first_n)
		Statsf("bucket size %d means bucket %d is [%d, %d)", kBUCKET_SIZE, largest_bucket,
			kBUCKET_SIZE*largest_bucket, kBUCKET_SIZE*(largest_bucket+1))
		Statsf("largest bucket: mean=%f sd=%f", first_mean, first_sd)
		Statsf("first bounds: [%d, %d]", first_bounds_min, first_bounds_max)
		Statsf("have %d servers within bounds, mean value %f sd=%f", len(first_ips_list), second_mean, second_sd)
	}

	if second_mean < float64(*flKeysSanityMin) {
		Statsf("mean %f < %d", second_mean, *flKeysSanityMin)
		abortMessage("broken_data")
		return
	}
	threshold_base_index := len(first_ips) - 2
	if threshold_base_index < 0 {
		threshold_base_index = 0
	}
	threshold_candidates := make([]int, 0, len(first_ips))
	for _, count := range first_ips {
		threshold_candidates = append(threshold_candidates, count)
	}
	sort.Ints(threshold_candidates)
	var threshold int = threshold_candidates[threshold_base_index] - (*flKeysDailyJitter + int(second_sd))

	if showStats {
		Statsf("Second largest count within bounds: %d", threshold_candidates[threshold_base_index])
		Statsf("threshold: %d", threshold)
	}

	if nt, ok := req.Form["threshold"]; ok {
		i, ok2 := strconv.Atoi(nt[0])
		if ok2 == nil && i > 0 {
			Statsf("Overriding threshold from CGI parameter; %d -> %d", threshold, i)
			threshold = i
		}
	}

	ips := make([]string, 0, len(first_ips_all))
	for ip, count := range first_ips_all {
		if count >= threshold {
			ips = append(ips, ip)
		}
	}
	if len(ips) == 0 {
		Statsf("No IPs above threshold %d", threshold)
		abortMessage("threshold_too_high")
		return
	}

	filterOut := func(rationale string, eliminate btree.SortedSet, eliminate_server_count int, candidates []string) []string {
		alreadyDropped := btree.NewTree(btreeStringLess)
		for ip := range eliminate.Data() {
			alreadyDropped.Insert(ip)
		}
		for _, ip := range candidates {
			alreadyDropped.Remove(ip)
		}
		ips = make([]string, 0, len(candidates))
		for _, ip := range candidates {
			if !eliminate.Contains(ip) {
				ips = append(ips, ip)
			}
		}
		Statsf("dropping all %d servers %s, for %d possible IPs but %d of those already dropped",
			eliminate_server_count, rationale, eliminate.Len(), alreadyDropped.Len())
		return ips
	}

	ips = filterOut("running version v1.0.10", ips_skip_1010, count_servers_1010, ips)
	if len(ips) == 0 {
		abortMessage("No_servers_left_after_v1.0.10_filter")
		return
	}

	if minimumVersion != nil {
		ips = filterOut(fmt.Sprintf("running version < v%s", minimumVersion), ips_too_old, count_servers_too_old, ips)
		if len(ips) == 0 {
			abortMessage(fmt.Sprintf("No_servers_left_after_minimum_version_filter_(v%s)", minimumVersion))
			return
		}
	}

	if limitToCountries != nil {
		ips = filterOut(fmt.Sprintf("not in countries [%s]", limitToCountries), ips_wrong_country, count_servers_wrong_country, ips)
		if len(ips) == 0 {
			abortMessage(fmt.Sprintf("No_servers_left_after_country_filter_[%s]", limitToCountries))
			return
		}
	}

	if limitToProxies {
		ips = filterOut("not behind a web-proxy", ips_unwanted_server, count_servers_unwanted_server, ips)
		if len(ips) == 0 {
			abortMessage("No_servers_left_after_proxies_filter")
			return
		}
	}

	//TODO: change now to be the time the scan finished
	timestamp := time.Now().UTC().Format("2006-01-02T15:04:05") + "Z"
	count := len(ips)
	Log.Printf("ip-valid: Yielding %d of %d values", count, len(ips_all))

	// The tags are public statements; history:
	//   skip 1.0.10 -> skip_1010, because of lookup problems biting gnupg
	//   alg_1 used a fixed threshold (too small to deal with jitter)
	//   alg_2 used stddev+jitter
	//   alg_3 fixed maximum bucket selection (was a code bug)
	//   alg_4 stopped double-counting servers with multiple IP addresses
	//   alg_5 keep 1.0.10 servers for long enough to calculate stats, drop afterwards
	statusD := make(map[string]interface{}, 16)
	statusD["status"] = "COMPLETE"
	statusD["count"] = count
	statusD["tags"] = []string{"skip_1010", "alg_5"}
	if minimumVersion != nil {
		statusD["minimum_version"] = minimumVersion.String()
	}
	if limitToProxies {
		statusD["proxies"] = "1"
	}
	if limitToCountries != nil {
		statusD["countries"] = limitToCountries.String()
	}
	statusD["minimum"] = threshold
	statusD["collected"] = timestamp

	if emitJson {
		fmt.Fprintf(w, "{\n")
		if showStats {
			doShowStats()
			fmt.Fprintf(w, ", ")
		}
		bIps, _ := json.Marshal(ips)
		bStatus, _ := json.Marshal(statusD)
		fmt.Fprintf(w, "\"status\": %s,\n\"ips\": %s\n}\n", bStatus, bIps)
	} else {
		if showStats {
			doShowStats()
		}
		fmt.Fprintf(w, "IP-Gen/1.1:")
		for k, v := range statusD {
			var vstr string
			//fmt.Fprintf(w, " {{%T}}", v)
			switch v.(type) {
			case int:
				vstr = strconv.Itoa(v.(int))
			case []string:
				vstr = strings.Join(v.([]string), ",")
			default:
				vstr = fmt.Sprintf("%s", v)
			}
			fmt.Fprintf(w, " %s=%s", k, vstr)
		}
		fmt.Fprintf(w, "\n")
		for _, ip := range ips {
			fmt.Fprintf(w, "%s\n", ip)
		}
		fmt.Fprintf(w, ".\n")
	}

}

func apiIpValidStatsPage(w http.ResponseWriter, req *http.Request) {
	var err error
	if err = req.ParseForm(); err != nil {
		http.Error(w, "Failed to parse form information", http.StatusBadRequest)
		return
	}
	req.Form.Set("stats", "1")
	apiIpValidPage(w, req)
}
