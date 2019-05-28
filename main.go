package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"net"
	"regexp"
	"strings"
	"time"
)

var (
	debug = flag.Bool("debug", false, "Print debug output")
	test  = flag.Bool("test", false, "do not send mails, print them instead")

	r = rand.New(rand.NewSource(time.Now().UnixNano()))
)

type keywordType struct {
	regexp      *regexp.Regexp
	keywordType string
	ipNet       *net.IPNet
	exceptions  []string
}

func debugOutput(s string, a ...interface{}) {
	if *debug {
		log.Printf("[DEBUG] %s", fmt.Sprintf(s, a...))
	}
}

func checkKeywords(body string, keywords *map[string]keywordType) (bool, map[string]string) {
	found := make(map[string]string)
	status := false
	for k, v := range *keywords {
		if v.regexp != nil {
			if s := v.regexp.FindStringSubmatch(body); s != nil {
				match := strings.TrimSpace(s[1])
				switch v.keywordType {
				case "ip":
					ip := net.ParseIP(match)
					// invalid IP matched
					if ip == nil {
						debugOutput("%q is not a valid ip", match)
						continue
					}
					if v.ipNet.Contains(ip) {
						debugOutput("%v contains %s", v.ipNet, ip)
						found[k] = match
						status = true
					}
				default:
					if !checkExceptions(match, v.exceptions) {
						found[k] = match
						status = true
					}
				}
			}
		}
	}
	return status, found
}

func checkExceptions(s string, exceptions []string) bool {
	for _, x := range exceptions {
		if strings.Contains(s, x) {
			debugOutput("String %q contains exception %q", s, x)
			return true
		}
	}
	return false
}

func parseKeywords(k []keyword) (*map[string]keywordType, error) {
	keywords := make(map[string]keywordType)
	// use a boundary for keyword searching
	for _, k := range k {
		switch k.Type {
		case "cidr":
			// capture IPs (only v4)
			// https://www.regular-expressions.info/ip.html
			r := `(\b(?:\d{1,3}\.){3}\d{1,3}\b)`
			_, n, err := net.ParseCIDR(k.Keyword)
			if err != nil {
				return nil, fmt.Errorf("could not parse cidr %s: %v", k.Keyword, err)
			}
			keywords[k.Keyword] = keywordType{
				regexp:      regexp.MustCompile(r),
				keywordType: "ip",
				ipNet:       n,
				exceptions:  k.Exceptions,
			}
		default:
			r := fmt.Sprintf(`(?im)^(.*\b%s.*)$`, regexp.QuoteMeta(k.Keyword))
			keywords[k.Keyword] = keywordType{
				regexp:      regexp.MustCompile(r),
				keywordType: "string",
				exceptions:  k.Exceptions,
			}
		}
	}
	return &keywords, nil
}

// nolint: gocyclo
func main() {
	configFile := flag.String("config", "", "Config File to use")
	var lastCheck time.Time

	chanError := make(chan error)
	chanOutput := make(chan paste)
	// we run in an endless loop so no need for a waitgroup here
	defer close(chanOutput)
	defer close(chanError)

	alredyChecked := make(map[string]time.Time)

	flag.Parse()

	log.Println("Starting Pastebin Scraper")
	config, err := getConfig(*configFile)
	if err != nil {
		log.Fatalf("could not read config file %s: %v", *configFile, err)
	}

	keywords, err := parseKeywords(config.Keywords)
	if err != nil {
		log.Fatalf("could not parse keywords: %v", err)
	}
	timeout, err := time.ParseDuration(config.Timeout)
	if err != nil {
		log.Fatalf("invalid value for timeout: %q - %v", config.Timeout, err)
	}
	client.Timeout = timeout

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func(c configuration) {
		for p := range chanOutput {
			debugOutput("found paste:\n%s", p)
			err = p.sendPasteMessage(c)
			if err != nil {
				chanError <- fmt.Errorf("sendPasteMessage: %v", err)
			}
		}
	}(*config)

	go func(c configuration) {
		for err := range chanError {
			log.Printf("%v", err)
			if c.Mailonerror {
				err2 := sendErrorMessage(c, err)
				if err2 != nil {
					log.Printf("ERROR on sending error mail: %v", err2)
				}
			}
		}
	}(*config)

	for {
		// Only fetch the main list once a minute
		sleepTime := time.Until(lastCheck.Add(1 * time.Minute))
		if sleepTime > 0 {
			debugOutput("sleeping for %s", sleepTime)
			time.Sleep(sleepTime)
		}

		lastCheck = time.Now()
		pastes, err := fetchPasteList(ctx)
		if err != nil {
			chanError <- fmt.Errorf("fetchPasteList: %v", err)
			continue
		}

		for _, p := range pastes {
			if _, ok := alredyChecked[p.Key]; ok {
				debugOutput("skipping key %s as it was already checked", p.Key)
			} else {
				alredyChecked[p.Key] = time.Now()
				p2, err := p.fetch(ctx, keywords)
				if err != nil {
					chanError <- fmt.Errorf("fetch: %v", err)
				} else if p2 != nil {
					chanOutput <- *p2
				}
				// do not hammer the API
				time.Sleep(1 * time.Second)
			}
		}
		// clean up old items in alreadyChecked map
		// delete everything older than 10 minutes
		threshold := time.Now().Add(-10 * time.Minute)
		for k, v := range alredyChecked {
			if v.Before(threshold) {
				debugOutput("deleting expired entry %s", k)
				delete(alredyChecked, k)
			}
		}
	}
}
