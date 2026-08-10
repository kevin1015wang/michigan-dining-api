// Command playwrightinstall installs just the Playwright Node driver (not the
// browsers themselves) that github.com/mxschmitt/playwright-go needs at
// runtime. It's used when building the fetch Docker image on top of a base
// image that already bundles a matching Chromium build (see Dockerfile.fetch),
// so the browser download is skipped and only the small driver package is
// fetched. Respects PLAYWRIGHT_DRIVER_PATH for where to install it.
package main

import (
	"log"

	"github.com/mxschmitt/playwright-go"
)

func main() {
	if err := playwright.Install(&playwright.RunOptions{SkipInstallBrowsers: true}); err != nil {
		log.Fatalf("could not install playwright driver: %v", err)
	}
}
