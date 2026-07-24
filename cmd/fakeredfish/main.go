package main

import (
	"flag"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

func main() {
	var (
		addr   = flag.String("addr", ":8443", "listen address")
		rack   = flag.String("rack", "r14", "rack name prefix")
		count  = flag.Int("count", 16, "number of simulated BMCs")
		faults = flag.String("faults", "", "comma-separated id=fault pairs, e.g. r14-3=ecc-storm")
		lag    = flag.Duration("apply-lag", 2*time.Second, "how long a firmware update takes to land")
	)
	flag.Parse()

	r := NewRack(*rack, *count)
	r.SetApplyLag(*lag)

	if *faults != "" {
		for _, pair := range strings.Split(*faults, ",") {
			kv := strings.SplitN(strings.TrimSpace(pair), "=", 2)
			if len(kv) != 2 {
				log.Printf("ignoring malformed fault spec %q", pair)
				continue
			}
			if err := r.Inject(kv[0], Fault(kv[1])); err != nil {
				log.Printf("inject %s: %v", pair, err)
				continue
			}
			log.Printf("injected fault %s on %s", kv[1], kv[0])
		}
	}

	srv := &http.Server{
		Addr:              *addr,
		Handler:           r.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	log.Printf("fakeredfish: rack %s with %d BMCs on %s", *rack, *count, *addr)
	log.Printf("available faults: %v", AllFaults())
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Printf("server error: %v", err)
		os.Exit(1)
	}
}
