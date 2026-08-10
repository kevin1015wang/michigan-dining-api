# Self-Hosting Guide

This is the practical path to run your own copy of this API, written after
converting the project off its old (now-broken) Bazel build and off the
University of Michigan JSON APIs it originally scraped (both are dead - see
below). Everything here has been built and run end-to-end locally with Docker
against a local DynamoDB instance and against the live dining.umich.edu site.

## What changed from the original repo

- **Build system**: converted from Bazel (pinned to a 2019 `rules_go` that no
  longer works with any current Bazel) to plain Go modules. `go build ./...`
  works with no other tooling required.
- **AWS SDK**: ported from a 2020 developer-preview version of `aws-sdk-go-v2`
  to the current stable SDK.
- **Data source**: UMich retired the two JSON APIs the original scraper used
  (`mobile.its.umich.edu` and the internal `webplatformsunpublished` endpoint -
  both are dead/unreachable now). The dining hall menus are still published as
  plain server-rendered HTML at `dining.umich.edu/menus-locations/...`, but the
  site is behind a Cloudflare bot check that a plain HTTP client can't pass.
  `cmd/fetch` now drives a real headless Chromium (via
  [playwright-go](https://github.com/mxschmitt/playwright-go)) to load each
  location page and parses the menu out of the HTML. See
  `api/mdining/mdiningscraper/`.
- Fixed a real bug where the app would `Fatalf` (crash the whole server) if
  DynamoDB Streams had a transient error - it now retries instead.

## 1. Set up AWS DynamoDB

DynamoDB is the only datastore this app uses, and it stays within AWS's
**Always Free tier** (25GB storage, 25 read/write capacity units) at this
app's scale indefinitely - hosting cost here is $0/month regardless of where
you run the web/fetch containers.

1. Create an AWS account if you don't have one.
2. In IAM, create a user for this app (programmatic access only, no console
   login needed).
3. Attach an inline policy using [`deploy/iam-policy.json`](deploy/iam-policy.json)
   in this repo - it's scoped to just the 6 tables this app uses
   (`DiningHalls`, `Items`, `Menus`, `Foods`, `FoodStats`, `Hearts`) rather
   than full DynamoDB access.
4. Create an access key for that user. You'll get an
   `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` pair - save these for step 3
   below.

Tables are created automatically the first time `web` or `fetch` runs
(`CreateTablesIfNotExists`), so there's no separate manual step.

## 2. Pick a host

Given the goal is low/no cost, in order of recommendation:

- **Oracle Cloud "Always Free" tier** - a genuinely free-forever VM (e.g. an
  Ampere A1 instance with several GB of RAM), which comfortably runs both the
  web server and the headless-Chromium fetch job. Signup requires a credit
  card for identity verification but you won't be charged if you stay within
  the free-tier shapes.
- **A cheap VPS** (Hetzner, DigitalOcean, Linode, ~$4-6/mo) if Oracle's signup
  is a hassle or unavailable to you.

Either way you just need: a Linux VM, Docker installed, and a domain (optional
- you can also just hit the server's IP on port 8081 or 443).

## 3. Deploy

On the server:

```shell
git clone <your fork of this repo>
cd michigan-dining-api
cp .env.example .env
# edit .env and fill in AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY from step 1
```

If you want a domain with automatic HTTPS, edit `Caddyfile` and replace
`your-domain.example.com` with your real domain (point its DNS A record at
the server's IP first). If you'd rather skip Caddy and just hit the server
directly, remove the `caddy` service from `docker-compose.yml` and use port
8081 directly.

```shell
docker compose up -d --build web caddy
```

This starts the REST/gRPC API server. It will come up with empty tables until
the first fetch runs (next step).

## 4. Run the fetch job

The `fetch` service scrapes dining.umich.edu and populates DynamoDB. It's not
part of `docker compose up` by design - it's a batch job, not a long-running
service - so run it once now to seed data:

```shell
docker compose run --rm fetch
```

This takes 1-2 minutes (it visits ~25 location pages with a polite delay
between each, on purpose, to look like a normal slow visitor rather than a
scraping burst).

Then schedule it daily via the host's crontab (`crontab -e`):

```
0 6 * * * cd /path/to/michigan-dining-api && docker compose run --rm fetch >> /var/log/mdining-fetch.log 2>&1
```

## Notes / things that may need attention later

- **The scraper is inherently more fragile than a real API.** It depends on
  dining.umich.edu's current HTML structure and its Cloudflare bot check
  continuing to let a real headless browser through (which is standard,
  expected behavior for that kind of challenge - a real browser is exactly
  what it's designed to admit). If UMich changes their page markup or
  tightens bot protection, `api/mdining/mdiningscraper/mdiningscraper.go`'s
  parsing logic or `Locations` list is the place to fix it.
- **The `Locations` list is hand-collected** from
  `dining.umich.edu/menus-locations/` as of 2026 (9 dining halls, 9 cafes, 7
  markets). If UMich adds/renames a location, it won't show up until you add
  it there.
- **Real-time occupancy/capacity data is gone.** That came from an internal
  UMich feed (`mdiningapi2`'s capacity endpoint) that was never public and
  isn't recoverable by scraping the public site.
- The `cmd/scrape` binary and `mdiningclient`/`mdiningclient2` packages are
  the old, now-dead API clients, left in place for reference but unused.
