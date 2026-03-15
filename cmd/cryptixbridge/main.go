package main

import (
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"path"
	"time"

	"github.com/cryptix-network/cryptix-stratum-bridge-v3/src/cryptixstratum"

	"gopkg.in/yaml.v2"
)

func main() {
	pwd, _ := os.Getwd()
	fullPath := path.Join(pwd, "config.yaml")
	log.Printf("loading config @ `%s`", fullPath)
	rawCfg, err := ioutil.ReadFile(fullPath)
	if err != nil {
		log.Printf("config file not found: %s", err)
		os.Exit(1)
	}
	cfg := cryptixstratum.BridgeConfig{
		StratumV2Port:     ":5556",
		StratumV2Fallback: false,
	}
	if err := yaml.Unmarshal(rawCfg, &cfg); err != nil {
		log.Printf("failed parsing config file: %s", err)
		os.Exit(1)
	}

	// override flag.Usage for better help output.
	flag.Usage = func() {
		flag.VisitAll(func(f *flag.Flag) {
			fmt.Fprintf(os.Stderr, "  -%v %v\n", f.Name, f.Value)
			fmt.Fprintf(os.Stderr, "    	%v (default \"%v\")\n", f.Usage, f.Value)
		})
	}

	flag.StringVar(&cfg.StratumPort, "stratum", cfg.StratumPort, "stratum port to listen on, default `:5555`")
	flag.BoolVar(&cfg.StratumV2Enabled, "stratumv2", cfg.StratumV2Enabled, "enable optional Stratum V2 listener")
	flag.StringVar(&cfg.StratumV2Port, "stratumv2port", cfg.StratumV2Port, "stratum v2 port to listen on when -stratumv2 is enabled")
	flag.BoolVar(&cfg.StratumV2Fallback, "stratumv2fallback", cfg.StratumV2Fallback, "when true, v2 listener allows v1 fallback on the v2 port")
	flag.BoolVar(&cfg.PrintStats, "stats", cfg.PrintStats, "true to show periodic stats to console, default `true`")
	flag.StringVar(&cfg.RPCServer, "cryptix", cfg.RPCServer, "address of the cryptix node, default `localhost:13110`")
	flag.DurationVar(&cfg.BlockWaitTime, "blockwait", cfg.BlockWaitTime, "time in ms to wait before manually requesting new block, default `500`")
	flag.Float64Var(&cfg.MinShareDiff, "mindiff", cfg.MinShareDiff, "minimum share difficulty to accept from miner(s), default `4`")
	flag.BoolVar(&cfg.VarDiff, "vardiff", cfg.VarDiff, "true to enable auto-adjusting variable min diff")
	flag.UintVar(&cfg.SharesPerMin, "sharespermin", cfg.SharesPerMin, "number of shares per minute the vardiff engine should target")
	flag.BoolVar(&cfg.VarDiffStats, "vardiffstats", cfg.VarDiffStats, "include vardiff stats readout every 10s in log")
	flag.BoolVar(&cfg.SoloMining, "solo", cfg.SoloMining, "true to use network diff instead of stratum vardiff")
	flag.UintVar(&cfg.ExtranonceSize, "extranonce", cfg.ExtranonceSize, "size in bytes of extranonce, default `0`")
	flag.StringVar(&cfg.PromPort, "prom", cfg.PromPort, "address to serve prom stats, default `:2112`")
	flag.BoolVar(&cfg.UseLogFile, "log", cfg.UseLogFile, "if true will output errors to log file, default `true`")
	flag.StringVar(&cfg.HealthCheckPort, "hcp", cfg.HealthCheckPort, `(rarely used) if defined will expose a health check on /readyz, default ""`)
	flag.Parse()

	if cfg.MinShareDiff == 0 {
		cfg.MinShareDiff = 4
	}
	if cfg.BlockWaitTime == 0 {
		cfg.BlockWaitTime = 5 * time.Second // this should never happen due to cryptix 1s block times
	}

	log.Println("----------------------------------")
	log.Printf("initializing bridge")
	log.Printf("\tcryptix:          %s", cfg.RPCServer)
	log.Printf("\tstratum:         %s", cfg.StratumPort)
	log.Printf("\tstratum v2:      %t", cfg.StratumV2Enabled)
	log.Printf("\tstratum v2 port: %s", cfg.StratumV2Port)
	log.Printf("\tv2 fallback v1:  %t", cfg.StratumV2Fallback)
	log.Printf("\tprom:            %s", cfg.PromPort)
	log.Printf("\tstats:           %t", cfg.PrintStats)
	log.Printf("\tlog:             %t", cfg.UseLogFile)
	log.Printf("\tmin diff:        %.10f", cfg.MinShareDiff)
	log.Printf("\tvar diff:        %t", cfg.VarDiff)
	log.Printf("\tshares per min:  %d", cfg.SharesPerMin)
	log.Printf("\tvar diff stats:  %t", cfg.VarDiffStats)
	log.Printf("\tsolo mining:  	 %t", cfg.SoloMining)
	log.Printf("\tblock wait:      %s", cfg.BlockWaitTime)
	log.Printf("\textranonce size: %d", cfg.ExtranonceSize)
	log.Printf("\thealth check:    %s", cfg.HealthCheckPort)
	log.Println("----------------------------------")

	if err := cryptixstratum.ListenAndServe(cfg); err != nil {
		log.Println(err)
	}
}
