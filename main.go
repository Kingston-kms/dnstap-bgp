package main

import (
	"flag"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/BurntSushi/toml"
	"fmt"
)

type cfgRoot struct {
	Domains string
	Cache   string
	Routers string
	TTL     string
	IPv6    bool

	DNSTap *dnstapCfg
}

var (
	version string
)

func main() {
	var (
		ipDB   *db
		err      error
		shutdown = make(chan struct{})
	)

	log.Printf("dnstap v%s", version)

	config := flag.String("config", "", "Path to a config file")
	flag.Parse()

	if *config == "" {
		log.Fatal("You need to specify path to a config file")
	}

	cfg := &cfgRoot{}
	if _, err = toml.DecodeFile(*config, &cfg); err != nil {
		log.Fatalf("Unable to parse config file '%s': %s", *config, err)
	}

	cfg.DNSTap.IPv6 = cfg.IPv6

	if cfg.Domains == "" {
		log.Fatal("You need to specify path to a domain list")
	}

	if cfg.Routers == "" {
		log.Fatal("You need to specify path to a router list")
	}

	ttl := 24 * time.Hour
	if cfg.TTL != "" {
		if ttl, err = time.ParseDuration(cfg.TTL); err != nil {
			log.Printf("Unable to parse TTL: %s", err)
		}
	}

	expireCb := func(e *cacheEntry) {
		log.Printf("%s (%s) expired", e.IP, e.Domain)

		if ipDB != nil {
			if err := ipDB.del(e.IP); err != nil {
			}
		}
	}

	ipCache := newCache(ttl, expireCb)
	dTree := newDomainTree()

	cnt, skip, err := dTree.loadFile(cfg.Domains)
	if err != nil {
		log.Fatalf("Unable to load domain list: %s", err)
	}

	if err := os.Truncate(cfg.Routers, 0); err != nil {
		//log.Fatalf("Unable to clear file router list: %s", err)
	}

	routerFile, err := os.Open(cfg.Routers)
	if err != nil {
		newRouterFile, err := os.Create(cfg.Routers)
		if err != nil {
			log.Fatalf("Unable to create router list: %s", err)
		}
		routerFile = newRouterFile
		log.Printf("Create router list file: %s", cfg.Routers)
	}

	if routerFile == nil {
		log.Fatal("Unable to open router list")
	}

	log.Printf("Domains loaded: %d, skipped: %d", cnt, skip)

	if cfg.Cache != "" {
		if ipDB, err = newDB(cfg.Cache); err != nil {
			log.Fatalf("Unable to init DB '%s': %s", cfg.Cache, err)
		}

		es, err := ipDB.fetchAll()
		if err != nil {
			log.Fatalf("Unable to load entries from DB: %s", err)
		}

		now := time.Now()
		i, j, k := 0, 0, 0
		for _, e := range es {
			if now.Sub(e.TS) >= ttl {
				if err := ipDB.del(e.IP); err != nil {
					continue
				}
				j++
				continue
			}

			if !dTree.has(e.Domain) {
				if err := ipDB.del(e.IP); err != nil {
					continue
				}
				k++
				continue
			}

			ipCache.add(e)
			routeStr := fmt.Sprintf("route %s reject;", e)

			if _, err := routerFile.WriteString(routeStr); err != nil {

			}
			// add route line
			i++
		}

		log.Printf("Loaded from DB: %d, expired: %d, vanished: %d", i, j, k)
	}

	ipDBPut := func(e *cacheEntry) {
		if ipDB == nil {
			return
		}

		if err := ipDB.add(e); err != nil {
			log.Printf("Unable to add (%s, %s) to DB: %s", e.IP, e.Domain, err)
		}
	}

	addEntry := func(e *cacheEntry, touch bool) bool {
		if ipCache.exists(e.IP, touch) {
			if touch {
				ipDBPut(e)
			}

			return false
		}

		ipCache.add(e)
		ipDBPut(e)
		return true
	}

	addHostCb := func(ip net.IP, domain string) {
		if !dTree.has(domain) {
			return
		}

		e := &cacheEntry{
			IP:     ip,
			Domain: domain,
			TS:     time.Now(),
		}

		addEntry(e, true)
	}

	dnsTapErrorCb := func(err error) {
		log.Printf("DNSTap error: %s", err)
	}

	if _, err = newDnstapServer(cfg.DNSTap, addHostCb, dnsTapErrorCb); err != nil {
		log.Fatalf("Unable to init DNSTap: %s", err)
	}

	log.Printf("Listening for DNSTap on: %s", cfg.DNSTap.Listen)

	go func() {
		sigchannel := make(chan os.Signal, 1)
		signal.Notify(sigchannel, syscall.SIGTERM, syscall.SIGHUP, os.Interrupt)

		for sig := range sigchannel {
			switch sig {
			case syscall.SIGHUP:
				if i, s, err := dTree.loadFile(cfg.Domains); err != nil {
					log.Printf("Unable to load file: %s", err)
				} else {
					log.Printf("Domains loaded: %d, skipped: %d", i, s)
				}

			case os.Interrupt, syscall.SIGTERM:
				close(shutdown)

			/*case syscall.SIGUSR1:
				log.Printf("IPs exported: %d, domains loaded: %d", ipCache.count(), dTree.count())*/
			}
		}
	}()

	<-shutdown

	defer routerFile.Close()

	if ipDB != nil {
		ipDB.close()
	}
}
