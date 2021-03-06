package main

import (
	"encoding/csv"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"strings"
	"time"
	"unicode"

	"github.com/dchest/safefile"

	"github.com/jedisct1/dlog"
	"github.com/jedisct1/go-minisign"
)

type SourceFormat int

const (
	SourceFormatV1 = iota
	SourceFormatV2
)

const (
	SourcesUpdateDelay = time.Duration(24) * time.Hour
)

type Source struct {
	url    string
	format SourceFormat
	in     string
}

func fetchFromCache(cacheFile string) (in string, delayTillNextUpdate time.Duration, err error) {
	fi, err := os.Stat(cacheFile)
	if err != nil {
		delayTillNextUpdate = time.Duration(0)
		return
	}
	elapsed := time.Since(fi.ModTime())
	if elapsed < SourcesUpdateDelay {
		dlog.Debugf("Cache file [%s] is still fresh", cacheFile)
		delayTillNextUpdate = SourcesUpdateDelay - elapsed
	} else {
		dlog.Debugf("Cache file [%s] needs to be refreshed", cacheFile)
		delayTillNextUpdate = time.Duration(0)
	}
	var bin []byte
	bin, err = ioutil.ReadFile(cacheFile)
	if err != nil {
		delayTillNextUpdate = time.Duration(0)
		return
	}
	in = string(bin)
	return
}

func fetchWithCache(url string, cacheFile string) (in string, cached bool, delayTillNextUpdate time.Duration, err error) {
	cached = false
	in, delayTillNextUpdate, err = fetchFromCache(cacheFile)
	if err == nil {
		dlog.Debugf("Delay till next update: %v", delayTillNextUpdate)
		cached = true
		return
	}
	var resp *http.Response
	dlog.Infof("Loading source information from URL [%s]", url)
	resp, err = http.Get(url)
	if err == nil && resp != nil && (resp.StatusCode < 200 || resp.StatusCode > 299) {
		err = fmt.Errorf("Webserver returned code %d", resp.StatusCode)
		return
	} else if err != nil {
		return
	} else if resp == nil {
		err = errors.New("Webserver returned an error")
		return
	}
	var bin []byte
	bin, err = ioutil.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return
	}
	err = nil
	in = string(bin)
	delayTillNextUpdate = SourcesUpdateDelay
	return
}

func AtomicFileWrite(file string, data []byte) error {
	return safefile.WriteFile(file, data, 0644)
}

type URLToPrefetch struct {
	url       string
	cacheFile string
	when      time.Time
}

func NewSource(url string, minisignKeyStr string, cacheFile string, formatStr string, refreshDelay time.Duration) (Source, []URLToPrefetch, error) {
	_ = refreshDelay
	source := Source{url: url}
	if formatStr == "v1" {
		source.format = SourceFormatV1
	} else if formatStr == "v2" {
		source.format = SourceFormatV2
	} else {
		return source, []URLToPrefetch{}, fmt.Errorf("Unsupported source format: [%s]", formatStr)
	}
	minisignKey, err := minisign.NewPublicKey(minisignKeyStr)
	if err != nil {
		return source, []URLToPrefetch{}, err
	}
	now := time.Now()
	urlsToPrefetch := []URLToPrefetch{}

	sigURL := url + ".minisig"
	in, cached, delayTillNextUpdate, err := fetchWithCache(url, cacheFile)
	urlsToPrefetch = append(urlsToPrefetch, URLToPrefetch{url: url, cacheFile: cacheFile, when: now.Add(delayTillNextUpdate)})

	sigCacheFile := cacheFile + ".minisig"
	sigStr, sigCached, sigDelayTillNextUpdate, sigErr := fetchWithCache(sigURL, sigCacheFile)
	urlsToPrefetch = append(urlsToPrefetch, URLToPrefetch{url: sigURL, cacheFile: sigCacheFile, when: now.Add(sigDelayTillNextUpdate)})

	if err != nil || sigErr != nil {
		if err == nil {
			err = sigErr
		}
		return source, urlsToPrefetch, err
	}

	signature, err := minisign.DecodeSignature(sigStr)
	if err != nil {
		os.Remove(cacheFile)
		os.Remove(sigCacheFile)
		return source, urlsToPrefetch, err
	}
	res, err := minisignKey.Verify([]byte(in), signature)
	if err != nil || !res {
		os.Remove(cacheFile)
		os.Remove(sigCacheFile)
		return source, urlsToPrefetch, err
	}
	if !cached {
		if err = AtomicFileWrite(cacheFile, []byte(in)); err != nil {
			dlog.Warnf("%s: %s", cacheFile, err)
		}
	}
	if !sigCached {
		if err = AtomicFileWrite(sigCacheFile, []byte(sigStr)); err != nil {
			dlog.Warnf("%s: %s", sigCacheFile, err)
		}
	}
	dlog.Noticef("Source [%s] loaded", url)
	source.in = in
	return source, urlsToPrefetch, nil
}

func (source *Source) Parse(prefix string) ([]RegisteredServer, error) {
	if source.format == SourceFormatV1 {
		return source.parseV1(prefix)
	} else if source.format == SourceFormatV2 {
		return source.parseV2(prefix)
	}
	dlog.Fatal("Unexpected source format")
	return []RegisteredServer{}, nil
}

func (source *Source) parseV1(prefix string) ([]RegisteredServer, error) {
	var registeredServers []RegisteredServer

	csvReader := csv.NewReader(strings.NewReader(source.in))
	records, err := csvReader.ReadAll()
	if err != nil {
		return registeredServers, nil
	}
	for lineNo, record := range records {
		if len(record) == 0 {
			continue
		}
		if len(record) < 14 {
			return registeredServers, fmt.Errorf("Parse error at line %d", 1+lineNo)
		}
		if lineNo == 0 {
			continue
		}
		name := prefix + record[0]
		serverAddrStr := record[10]
		providerName := record[11]
		serverPkStr := record[12]
		props := ServerInformalProperties(0)
		if strings.EqualFold(record[7], "yes") {
			props |= ServerInformalPropertyDNSSEC
		}
		if strings.EqualFold(record[8], "yes") {
			props |= ServerInformalPropertyNoLog
		}
		stamp, err := NewDNSCryptServerStampFromLegacy(serverAddrStr, serverPkStr, providerName, props)
		if err != nil {
			return registeredServers, err
		}
		registeredServer := RegisteredServer{
			name: name, stamp: stamp,
		}
		dlog.Debugf("Registered [%s] with stamp [%s]", name, stamp.String())
		registeredServers = append(registeredServers, registeredServer)
	}
	return registeredServers, nil
}

func (source *Source) parseV2(prefix string) ([]RegisteredServer, error) {
	var registeredServers []RegisteredServer
	in := string(source.in)
	parts := strings.Split(in, "## ")
	if len(parts) < 2 {
		return registeredServers, fmt.Errorf("Invalid format for source at [%s]", source.url)
	}
	parts = parts[1:]
	for _, part := range parts {
		part = strings.TrimFunc(part, unicode.IsSpace)
		subparts := strings.Split(part, "\n")
		if len(subparts) < 2 {
			return registeredServers, fmt.Errorf("Invalid format for source at [%s]", source.url)
		}
		name := strings.TrimFunc(subparts[0], unicode.IsSpace)
		if len(name) == 0 {
			return registeredServers, fmt.Errorf("Invalid format for source at [%s]", source.url)
		}
		var stampStr string
		for _, subpart := range subparts {
			subpart = strings.TrimFunc(subpart, unicode.IsSpace)
			if strings.HasPrefix(subpart, "sdns://") {
				stampStr = subpart
				break
			}
		}
		if len(stampStr) < 8 {
			return registeredServers, fmt.Errorf("Missing stamp for server [%s] in source from [%s]", name, source.url)
		}
		stamp, err := NewServerStampFromString(stampStr)
		if err != nil {
			return registeredServers, err
		}
		registeredServer := RegisteredServer{
			name: name, stamp: stamp,
		}
		dlog.Debugf("Registered [%s] with stamp [%s]", name, stamp.String())
		registeredServers = append(registeredServers, registeredServer)
	}
	return registeredServers, nil
}

func PrefetchSourceURL(urlToPrefetch *URLToPrefetch) error {
	in, _, delayTillNextUpdate, err := fetchWithCache(urlToPrefetch.url, urlToPrefetch.cacheFile)
	if err == nil {
		AtomicFileWrite(urlToPrefetch.cacheFile, []byte(in))
	}
	urlToPrefetch.when = time.Now().Add(delayTillNextUpdate)
	return err
}
