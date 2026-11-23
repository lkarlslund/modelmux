// ccroute lets Claude Code use foreign models transparently. Point
// ANTHROPIC_BASE_URL at it: Messages API requests for a model listed in the
// config go to that model's server (Anthropic- or OpenAI-compatible) under the
// name Claude Code knows it by; everything else is passed through to
// api.anthropic.com byte for byte, with the client's own headers and
// credentials.
//
//	ccroute [serve] [-config FILE]     run the proxy
//	ccroute install [-config FILE]     set up Claude Code, agents and the user service from the config
//	ccroute uninstall                  undo install
//	ccroute check [-config FILE] [ID]  send a one-line prompt to each model through the proxy logic
//	ccroute env [-config FILE]         print the environment install would put in settings.json
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
)

func main() {
	log.SetFlags(0)
	args := os.Args[1:]
	cmd := "serve"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	var err error
	switch cmd {
	case "serve":
		err = serve(args)
	case "install":
		err = install(args)
	case "uninstall":
		err = uninstall(args)
	case "check":
		err = check(args)
	case "env":
		err = printEnv(args)
	case "help":
		fmt.Fprintln(os.Stderr, usageText)
	default:
		err = fmt.Errorf("unknown command %q\n%s", cmd, usageText)
	}
	if err != nil {
		log.Fatal("ccroute: ", err)
	}
}

const usageText = `usage:
  ccroute [serve] [-config FILE] [-listen ADDR] [-route PATTERN=UPSTREAM[,MODEL]]... [-dump DIR]
  ccroute install [-config FILE] [-settings FILE] [-agents DIR] [-no-service] [-dry-run]
  ccroute uninstall [-settings FILE] [-no-service]
  ccroute check [-config FILE] [MODEL_ID]...
  ccroute env [-config FILE]`

func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	cfgPath := fs.String("config", "", "config file (default "+defaultConfigPath()+" if it exists)")
	listen := fs.String("listen", "", "listen address (overrides the config)")
	dump := fs.String("dump", "", "directory to write translated request bodies to")
	var routes []string
	fs.Func("route", "PATTERN=UPSTREAM[,MODEL]: route models matching PATTERN (a prefix, or a glob with *) to an Anthropic-compatible UPSTREAM; repeatable", func(s string) error {
		routes = append(routes, s)
		return nil
	})
	_ = fs.Parse(args)

	cfg := &Config{}
	p := *cfgPath
	if p == "" && len(routes) == 0 {
		p = defaultConfigPath()
	}
	if p != "" {
		var err error
		if cfg, err = loadConfig(p); err != nil {
			return err
		}
	}
	for _, r := range routes {
		m, err := routeFlag(r)
		if err != nil {
			return err
		}
		cfg.Models = append(cfg.Models, m)
	}
	if *listen != "" {
		cfg.Listen = *listen
	}
	if *dump != "" {
		cfg.Dump = *dump
	}
	if err := cfg.validate(); err != nil {
		return err
	}
	px, err := newProxy(cfg)
	if err != nil {
		return err
	}
	log.SetFlags(log.LstdFlags)
	log.Printf("listening on %s, default %s, %d foreign model(s)", cfg.Listen, cfg.Anthropic, len(cfg.Models))
	for _, m := range cfg.Models {
		if _, err := m.key(); err != nil {
			log.Printf("warning: %v", err)
		}
	}
	return http.ListenAndServe(cfg.Listen, px)
}

// routeFlag parses the old -route PATTERN=UPSTREAM[,MODEL] flag. A pattern
// without a * is a prefix, as before config files existed.
func routeFlag(s string) (*Model, error) {
	pat, rest, ok := strings.Cut(s, "=")
	if !ok || pat == "" {
		return nil, fmt.Errorf("-route: want PATTERN=UPSTREAM[,MODEL], got %q", s)
	}
	up, model, _ := strings.Cut(rest, ",")
	if !strings.Contains(pat, "*") {
		pat += "*"
	}
	return &Model{ID: strings.TrimSuffix(pat, "*"), Match: []string{pat}, Upstream: up, UpstreamModel: model, Hidden: true}, nil
}
