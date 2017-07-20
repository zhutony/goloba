package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"sync"
	"time"

	yaml "gopkg.in/yaml.v2"

	"github.com/hnakamur/ltsvlog"
	"github.com/masa23/goloba/api"
)

const (
	usage = `Usage argtest [GlobalOptions] <Command> [Options]
Commands:
  info     show information
  detach   manually detach destination
  attach   manually attach destination

Globals Options:
`
	subcommandOptionsUsageFormat = "\nOptions for subcommand \"%s\":\n"
)

type cliApp struct {
	config     *cliConfig
	httpClient *http.Client
}

type cliConfig struct {
	Timeout    time.Duration     `yaml:"timeout"`
	APIServers []apiServerConfig `yaml:"api_servers"`
}

type apiServerConfig struct {
	URL string `yaml:"url"`
}

func main() {
	config := flag.String("config", "/etc/goloba/golobactl.yml", "config file")
	flag.Usage = func() {
		fmt.Print(usage)
		flag.PrintDefaults()
	}
	flag.Parse()
	args := flag.Args()
	if len(args) == 0 {
		flag.Usage()
		os.Exit(1)
	}

	conf, err := loadConfig(*config)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to load config file; %v\n", err)
		os.Exit(1)
	}

	app := &cliApp{
		config:     conf,
		httpClient: &http.Client{Timeout: conf.Timeout},
	}
	switch args[0] {
	case "info":
		app.infoCommand(args[1:])
	case "detach":
		app.detachCommand(args[1:])
	case "attach":
		app.attachCommand(args[1:])
	case "unlock":
		app.unlockCommand(args[1:])
	default:
		flag.Usage()
		os.Exit(1)
	}
}

func loadConfig(file string) (*cliConfig, error) {
	buf, err := ioutil.ReadFile(file)
	if err != nil {
		return nil, ltsvlog.WrapErr(err, func(err error) error {
			return fmt.Errorf("failed to read config file, err=%v", err)
		}).String("configFile", file).Stack("")
	}
	var c cliConfig
	err = yaml.Unmarshal(buf, &c)
	if err != nil {
		return nil, ltsvlog.WrapErr(err, func(err error) error {
			return fmt.Errorf("failed to parse config file, err=%v", err)
		}).String("configFile", file).Stack("")
	}
	return &c, nil
}

func subcommandUsageFunc(subcommand string, fs *flag.FlagSet) func() {
	return func() {
		flag.Usage()
		fmt.Printf(subcommandOptionsUsageFormat, subcommand)
		fs.PrintDefaults()
	}
}

func (a *cliApp) infoCommand(args []string) {
	fs := flag.NewFlagSet("info", flag.ExitOnError)
	fs.Usage = subcommandUsageFunc("info", fs)
	format := fs.String("format", "text", "result format, 'text' or 'json'")
	fs.Parse(args)

	var wg sync.WaitGroup
	for _, s := range a.config.APIServers {
		wg.Add(1)
		s := s
		go func() {
			defer wg.Done()

			u := fmt.Sprintf("%s/info", s.URL)
			resp, err := a.httpClient.Get(u)
			if err != nil {
				fmt.Fprintf(os.Stderr, "failed to send request; %v\n", err)
				return
			}
			defer resp.Body.Close()

			data, err := ioutil.ReadAll(resp.Body)
			if err != nil {
				ltsvlog.Err(ltsvlog.WrapErr(err, func(err error) error {
					return fmt.Errorf("failed to read response from goloba API server")
				}).String("serverURL", s.URL).Stack(""))
			}
			switch *format {
			case "json":
				fmt.Printf("%s:\n%s\n", s.URL, string(data))
			case "text":
				var info api.Info
				err = json.Unmarshal(data, &info)
				if err != nil {
					ltsvlog.Err(ltsvlog.WrapErr(err, func(err error) error {
						return fmt.Errorf("failed to unmarshal JSON response from goloba API server")
					}).String("serverURL", s.URL).Stack(""))
				}
				// ipvsadm output:
				// [root@lbvm01 ~]# ipvsadm -Ln
				// IP Virtual Server version 1.2.1 (size=4096)
				// Prot LocalAddress:Port Scheduler Flags
				//   -> RemoteAddress:Port           Forward Weight ActiveConn InActConn
				// TCP  192.168.122.2:80 wrr
				//   -> 192.168.122.62:80            Route   100    0          0
				//   -> 192.168.122.240:80           Route   500    0          0
				// TCP  192.168.122.2:443 wrr
				//   -> 192.168.122.62:443           Masq    10     0          0
				//   -> 192.168.122.240:443          Masq    20     0          0
				//
				// goloba output:
				// [root@lbvm01 ~]# curl localhost:8880/info
				// Prot LocalAddress:Port Scheduler Flags
				//   -> RemoteAddress:Port           Forward Weight ActiveConn InActConn Detached Locked
				// tcp  192.168.122.2:80 wrr
				//   -> 192.168.122.62:80            droute  100    0          0         true     false
				//   -> 192.168.122.240:80           droute  500    0          0         false    false
				// tcp  192.168.122.2:443 wrr
				//   -> 192.168.122.62:443           masq    10     0          0         true     false
				//   -> 192.168.122.240:443          masq    20     0          0         false    false
				fmt.Printf("%s:\n", s.URL)
				fmt.Printf("Prot LocalAddress:Port Scheduler Flags\n")
				fmt.Printf("  -> RemoteAddress:Port           Forward Weight ActiveConn InActConn Detached Locked\n")
				for _, sr := range info.Services {
					fmt.Printf("%-4s %s:%d %s\n", sr.Protocol, sr.Address, sr.Port, sr.Schedule)
					for _, d := range sr.Destinations {
						hostPort := net.JoinHostPort(d.Address, strconv.Itoa(int(d.Port)))
						fmt.Printf("  -> %-28s %-7s %-6d %-10d %-9d %-8v %v\n", hostPort, d.Forward, d.Weight, d.ActiveConn, d.InactiveConn, d.Detached, d.Locked)
					}
				}
				fmt.Println()
			}
		}()
	}
	wg.Wait()
}

func (a *cliApp) attachCommand(args []string) {
	fs := flag.NewFlagSet("attach", flag.ExitOnError)
	fs.Usage = subcommandUsageFunc("attach", fs)
	serviceAddr := fs.String("s", "", "service address in <IPAddress>:<port> form")
	destAddr := fs.String("d", "", "destination address in <IPAddress>:<port> form")
	lock := fs.Bool("lock", true, "lock attach or detach regardless of future healthcheck results")
	fs.Parse(args)

	var wg sync.WaitGroup
	for _, s := range a.config.APIServers {
		wg.Add(1)
		s := s
		go func() {
			defer wg.Done()

			u := fmt.Sprintf("%s/attach?service=%s&dest=%s&lock=%v",
				s.URL, url.QueryEscape(*serviceAddr), url.QueryEscape(*destAddr), *lock)
			resp, err := a.httpClient.Get(u)
			if err != nil {
				fmt.Fprintf(os.Stderr, "failed to send request; %v\n", err)
				return
			}
			defer resp.Body.Close()

			data, err := ioutil.ReadAll(resp.Body)
			if err != nil {
				ltsvlog.Err(ltsvlog.WrapErr(err, func(err error) error {
					return fmt.Errorf("failed to read response from goloba API server")
				}).String("serverURL", s.URL).Stack(""))
			}
			fmt.Printf("%s:\n%s\n", s.URL, string(data))
		}()
	}
	wg.Wait()
}

func (a *cliApp) detachCommand(args []string) {
	fs := flag.NewFlagSet("detach", flag.ExitOnError)
	fs.Usage = subcommandUsageFunc("detach", fs)
	serviceAddr := fs.String("s", "", "service address in <IPAddress>:<port> form")
	destAddr := fs.String("d", "", "destination address in <IPAddress>:<port> form")
	lock := fs.Bool("lock", true, "lock detach or detach regardless of future healthcheck results")
	fs.Parse(args)

	var wg sync.WaitGroup
	for _, s := range a.config.APIServers {
		wg.Add(1)
		s := s
		go func() {
			defer wg.Done()

			u := fmt.Sprintf("%s/detach?service=%s&dest=%s&lock=%v",
				s.URL, url.QueryEscape(*serviceAddr), url.QueryEscape(*destAddr), *lock)
			resp, err := a.httpClient.Get(u)
			if err != nil {
				fmt.Fprintf(os.Stderr, "failed to send request; %v\n", err)
				return
			}
			defer resp.Body.Close()

			data, err := ioutil.ReadAll(resp.Body)
			if err != nil {
				ltsvlog.Err(ltsvlog.WrapErr(err, func(err error) error {
					return fmt.Errorf("failed to read response from goloba API server")
				}).String("serverURL", s.URL).Stack(""))
			}
			fmt.Printf("%s:\n%s\n", s.URL, string(data))
		}()
	}
	wg.Wait()
}

func (a *cliApp) unlockCommand(args []string) {
	fs := flag.NewFlagSet("unlock", flag.ExitOnError)
	fs.Usage = subcommandUsageFunc("unlock", fs)
	serviceAddr := fs.String("s", "", "service address in <IPAddress>:<port> form")
	destAddr := fs.String("d", "", "destination address in <IPAddress>:<port> form")
	fs.Parse(args)

	var wg sync.WaitGroup
	for _, s := range a.config.APIServers {
		wg.Add(1)
		s := s
		go func() {
			defer wg.Done()

			u := fmt.Sprintf("%s/unlock?service=%s&dest=%s",
				s.URL, url.QueryEscape(*serviceAddr), url.QueryEscape(*destAddr))
			resp, err := a.httpClient.Get(u)
			if err != nil {
				fmt.Fprintf(os.Stderr, "failed to send request; %v\n", err)
				return
			}
			defer resp.Body.Close()

			io.Copy(os.Stdout, resp.Body)
		}()
	}
	wg.Wait()
}