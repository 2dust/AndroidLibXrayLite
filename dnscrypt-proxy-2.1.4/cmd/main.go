package main

import (
	"log"
	"sync"
	"time"

	"gitlab.com/nebulavpn/dnscrypt_proxy/dnscrypt_lib"
)

var quit chan struct{}
var wg sync.WaitGroup

func main() {
	config := `# Empty listen_addresses to use systemd socket activation
	listen_addresses = ['0.0.0.0:53']
	server_names = ['loophole', 'cfloophole', 'cf-osmiran', 'dl3-loophole']
	max_clients = 1024
	ipv6_servers = false
	ipv4_servers = true
	
	
	lb_strategy = 'p2'
	
	# log_file = '/var/log/dnscrypt-proxy/main.log'
	
	cache_size = 4096
	cache_min_ttl = 2400
	cache_max_ttl = 86400
	cache_neg_min_ttl = 60
	cache_neg_max_ttl = 600
	
	
	[query_log]
	  file = '/tmp/dnscrypt-query.log'
	
	[nx_log]
	  file = '/var/log/dnscrypt-proxy/nx.log'
	
	[sources]
	  [sources.'public-resolvers']
	  url = 'https://download.dnscrypt.info/resolvers-list/v2/public-resolvers.md'
	  cache_file = '/var/cache/dnscrypt-proxy/public-resolvers.md'
	  minisign_key = 'RWQf6LRCGA9i53mlYecO4IzT51TGPpvWucNSCh1CBM0QTaLn73Y7GFO3'
	  refresh_delay = 72
	  prefix = ''
	
	[static]
	  [static.'loophole']
	  stamp = 'sdns://AgcAAAAAAAAADTk1LjIxNy4yMy4xMDUAE2FyY2hpdmUubG9vcGhvbGUuaXIUL2JyZWFrLXJlc29sdmVyLWZyZWU'
	
	  [static.'cfloophole']
	  stamp = 'sdns://AgcAAAAAAAAADDE4OC4xMTQuOTcuMwASdmlkZW9zLmxvb3Bob2xlLmlyFC9icmVhay1yZXNvbHZlci1mcmVl'
	
	  [static.'cf-osmiran']
	  stamp = 'sdns://AgcAAAAAAAAADTEwNC4yMS44MC4xOTEAEWZvcnVtLm9zbWlyYW4ub3JnMi90aGlzLWlzLWFsc28tc3VwcG9zZWQtdG8tbG9vay1saWtlLWEtbWFwLXJlc291cmNl'
	
	  [static.'dl3-loophole']
	  stamp = 'sdns://AgcAAAAAAAAADzE5NC4yMzMuMTYzLjE3NAAPZGwzLmxvb3Bob2xlLmlyMi90aGlzLWlzLWFsc28tc3VwcG9zZWQtdG8tbG9vay1saWtlLWEtbWFwLXJlc291cmNl'
	`

	quit = make(chan struct{})
	wg.Add(1)

	ps := dnscrypt_lib.Start(config)

	go func() {
		time.Sleep(time.Second * 10)
		quit <- struct{}{}
	}()

	<-quit
	log.Println("Quit signal received...")
	wg.Done()

	ps.Stop()

	time.Sleep(time.Second * 10)
	main()
}
