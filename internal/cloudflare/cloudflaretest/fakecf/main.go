// Command fakecf serves the in-memory Cloudflare stand-in, to try ekPanel's DNS features
// without a Cloudflare account: run it, then start the panel with
// EKPANEL_CLOUDFLARE_API=http://<address>/client/v4 and connect with the token below.
package main

import (
	"flag"
	"log"
	"net/http"
	"strings"

	"ekpanel/internal/cloudflare/cloudflaretest"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:18080", "address to listen on")
	token := flag.String("token", "test-token", "the API token it accepts")
	zones := flag.String("zones", "", "zones to start with, separated by commas")
	flag.Parse()
	fake := cloudflaretest.New(*token)
	for _, z := range strings.Split(*zones, ",") {
		if z = strings.TrimSpace(z); z != "" {
			fake.AddZone(z)
		}
	}
	log.Printf("fake Cloudflare API on http://%s/client/v4 (token %q)", *listen, *token)
	log.Fatal(http.ListenAndServe(*listen, fake))
}
