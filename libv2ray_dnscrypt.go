package libv2ray

import (
	crypto_rand "crypto/rand"
	"encoding/binary"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"runtime"
	"sync"

	"github.com/dnscrypt/dnscrypt-proxy/dnscrypt_lib"
	"github.com/jedisct1/dlog"
	"github.com/kardianos/service"
)

var quit chan bool

type App struct {
	wg        sync.WaitGroup
	quit      chan struct{}
	proxy     *dnscrypt_lib.Proxy
	flags     *dnscrypt_lib.ConfigFlags
	configStr string
}

func DnscryptStart(configStr string) {
	quit = make(chan bool)
	go func() {
		for {
			select {
			case <-quit:
				return
			default:
				helper(configStr)
			}
		}
	}()
}

func helper(configStr string) {
	dnscrypt_lib.TimezoneSetup()
	dlog.Init("dnscrypt-proxy", dlog.SeverityNotice, "DAEMON")
	runtime.MemProfileRate = 0

	seed := make([]byte, 8)
	crypto_rand.Read(seed)
	rand.Seed(int64(binary.LittleEndian.Uint64(seed[:])))

	pwd, err := os.Getwd()
	if err != nil {
		dlog.Fatal("Unable to find the path to the current directory")
	}

	emptyStr := ""
	falseBool := false
	timeout := 60
	configName := "config.yml"

	svcFlag := &emptyStr
	version := &falseBool
	flags := dnscrypt_lib.ConfigFlags{}
	flags.Resolve = &emptyStr
	flags.List = &falseBool
	flags.ListAll = &falseBool
	flags.JSONOutput = &falseBool
	flags.Check = &falseBool
	flags.ConfigFile = &configName
	flags.Child = &falseBool
	flags.NetprobeTimeoutOverride = &timeout
	flags.ShowCerts = &falseBool

	flag.Parse()

	if *version {
		fmt.Println(dnscrypt_lib.AppVersion)
		os.Exit(0)
	}

	app := &App{
	    configStr: configStr,
		flags: &flags,
	}

	svcConfig := &service.Config{
		Name:             "dnscrypt-proxy",
		DisplayName:      "DNSCrypt client proxy",
		Description:      "Encrypted/authenticated DNS proxy",
		WorkingDirectory: pwd,
		Arguments:        []string{"-config", *flags.ConfigFile},
	}
	svc, err := service.New(app, svcConfig)
	if err != nil {
		svc = nil
		dlog.Debug(err)
	}

	app.proxy = dnscrypt_lib.NewProxy()
	_ = dnscrypt_lib.ServiceManagerStartNotify()
	if len(*svcFlag) != 0 {
		if svc == nil {
			dlog.Fatal("Built-in service installation is not supported on this platform")
		}
		if err := service.Control(svc, *svcFlag); err != nil {
			dlog.Fatal(err)
		}
		if *svcFlag == "install" {
			dlog.Notice("Installed as a service. Use `-service start` to start")
		} else if *svcFlag == "uninstall" {
			dlog.Notice("Service uninstalled")
		} else if *svcFlag == "start" {
			dlog.Notice("Service started")
		} else if *svcFlag == "stop" {
			dlog.Notice("Service stopped")
		} else if *svcFlag == "restart" {
			dlog.Notice("Service restarted")
		}
		return
	}
	if svc != nil {
		if err := svc.Run(); err != nil {
			dlog.Fatal(err)
		}
	} else {
		app.Start(nil)
	}
}

func (app *App) Start(service service.Service) error {
	if service != nil {
		go func() {
			app.AppMain()
		}()
	} else {
		app.AppMain()
	}
	return nil
}

func (app *App) AppMain() {
	if err := dnscrypt_lib.ConfigLoad(app.proxy, app.flags, app.configStr); err != nil {
		dlog.Fatal(err)
	}
	if err := dnscrypt_lib.PidFileCreate(); err != nil {
		dlog.Criticalf("Unable to create the PID file: %v", err)
	}
	if err := app.proxy.InitPluginsGlobals(); err != nil {
		dlog.Fatal(err)
	}
	app.quit = make(chan struct{})
	app.wg.Add(1)
	app.proxy.StartProxy()
	runtime.GC()
	<-app.quit
	dlog.Notice("Quit signal received...")
	app.wg.Done()
}

func (app *App) Stop(service service.Service) error {
	dnscrypt_lib.PidFileRemove()
	dlog.Notice("Stopped.")
	return nil
}

func DnscryptStop() {
	dnscrypt_lib.PidFileRemove()
	dlog.Notice("Stopped.")
	quit <- true
	os.Exit(1)
}
