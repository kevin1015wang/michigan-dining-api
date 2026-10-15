// Package mdiningscraper fetches menu data by rendering and parsing the public
// dining.umich.edu location pages. UMich retired the JSON APIs the old clients
// (mdiningclient, mdiningclient2) used to hit, and the site is now behind a
// Cloudflare bot check that a plain HTTP client can't pass, so this drives a
// real headless browser and scrapes the server-rendered menu markup instead.
//
// The site now also serves a Cloudflare Turnstile challenge that a local
// headless Chromium can't clear. If BROWSERLESS_TOKEN is set, the scraper
// instead has Browserless (https://docs.browserless.io/examples/cloudflare)
// solve the challenge in a remote browser and connects to that browser over
// CDP, so every page load reuses the session's clearance cookies.
package mdiningscraper

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/MichiganDiningAPI/internal/util/date"
	pb "github.com/MichiganDiningAPI/proto/mdining"
	"github.com/PuerkitoBio/goquery"
	"github.com/golang/glog"
	"github.com/mxschmitt/playwright-go"
)

var (
	valueUnitRegex    = regexp.MustCompile(`^([\d\.]+)\s*([a-zA-Z%]*)$`)
	percentDailyRegex = regexp.MustCompile(`(\d+)%?`)
)

const (
	baseURL = "https://dining.umich.edu/menus-locations/"

	DiningHallsGroup = "DINING HALLS"
	CafesGroup       = "CAFES"
	MarketsGroup     = "MARKETS"

	userAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36"
)

// Location is a single dining.umich.edu menu page to scrape.
type Location struct {
	URLPath string
	Group   string
}

// Locations lists every dining hall, cafe, and market with a menu page on
// dining.umich.edu as of 2026. UMich adds/removes/renames these occasionally;
// re-derive this list from https://dining.umich.edu/menus-locations/ if scraping
// starts silently skipping a location.
var Locations = []Location{
	{URLPath: "dining-halls/bursley", Group: DiningHallsGroup},
	{URLPath: "dining-halls/east-quad", Group: DiningHallsGroup},
	{URLPath: "dining-halls/markley", Group: DiningHallsGroup},
	{URLPath: "dining-halls/mosher-jordan", Group: DiningHallsGroup},
	{URLPath: "dining-halls/north-quad", Group: DiningHallsGroup},
	{URLPath: "dining-halls/south-quad", Group: DiningHallsGroup},
	{URLPath: "dining-halls/twigs-at-oxford", Group: DiningHallsGroup},
	{URLPath: "dining-halls/wolverine-village-dining-hall", Group: DiningHallsGroup},
	{URLPath: "dining-halls/select-access/lawyers-club", Group: DiningHallsGroup},
	{URLPath: "dining-halls/select-access/martha-cook", Group: DiningHallsGroup},
	{URLPath: "cafes/berts-cafe", Group: CafesGroup},
	{URLPath: "cafes/cafe-32", Group: CafesGroup},
	{URLPath: "cafes/darwins", Group: CafesGroup},
	{URLPath: "cafes/east-quad", Group: CafesGroup},
	{URLPath: "cafes/eigen-cafe", Group: CafesGroup},
	{URLPath: "cafes/fireside-cafe", Group: CafesGroup},
	{URLPath: "cafes/mujo-cafe", Group: CafesGroup},
	{URLPath: "cafes/taubman-health-sciences-library", Group: CafesGroup},
	{URLPath: "cafes/umma-cafe", Group: CafesGroup},
	{URLPath: "markets/blue-market-and-cafe/mosher-jordan", Group: MarketsGroup},
	{URLPath: "markets/blue-market/bursley", Group: MarketsGroup},
	{URLPath: "markets/blue-market/michigan-union", Group: MarketsGroup},
	{URLPath: "markets/blue-market/munger", Group: MarketsGroup},
	{URLPath: "markets/blue-market/pierpont-commons", Group: MarketsGroup},
	{URLPath: "markets/maizies", Group: MarketsGroup},
	{URLPath: "markets/pharm-fresh", Group: MarketsGroup},
}

const (
	browserlessTokenEnv = "BROWSERLESS_TOKEN"
	browserlessURL      = "https://production-sfo.browserless.io"

	// browserlessSessionTTL is how long (ms) Browserless keeps an unblocked
	// browser alive for us to connect to. The current plan caps this at 60s.
	browserlessSessionTTL = 60 * 1000

	// browserlessSessionMaxAge is how long the scraper uses one session before
	// proactively swapping in a fresh one. Sessions die about a TTL after
	// they're created, connected or not, and a full run takes ~15-20 minutes,
	// so rotating ahead of that avoids page loads failing mid-run.
	browserlessSessionMaxAge = 45 * time.Second

	// browserlessUnblockAttempts bounds retries of the /unblock call itself,
	// which returns 429 when the plan's concurrent-session limit is hit (e.g.
	// a just-closed session hasn't been released yet) and 408 when it can't
	// get through the challenge in time.
	browserlessUnblockAttempts = 4
)

// Scraper drives a headless browser to render and scrape dining.umich.edu pages.
type Scraper struct {
	pw      *playwright.Playwright
	browser playwright.Browser
	page    playwright.Page

	// browserlessToken is empty when running a local Chromium.
	browserlessToken string
	// sessionStart is when the current Browserless session was connected.
	sessionStart time.Time
}

// New starts the browser the scraper renders pages with. Call Close when done.
//
// With BROWSERLESS_TOKEN set, it connects to a remote Browserless browser that
// has already passed the Cloudflare challenge. Otherwise it launches a local
// headless Chromium, which requires the Playwright browser binaries; if
// missing, run
// `go run github.com/mxschmitt/playwright-go/cmd/playwright install --with-deps chromium`.
func New() (*Scraper, error) {
	pw, err := playwright.Run()
	if err != nil {
		return nil, fmt.Errorf("could not start playwright: %w", err)
	}
	s := &Scraper{pw: pw, browserlessToken: os.Getenv(browserlessTokenEnv)}
	if s.browserlessToken != "" {
		err = s.connectBrowserless()
	} else {
		err = s.launchLocal()
	}
	if err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

func (s *Scraper) launchLocal() error {
	browser, err := s.pw.Chromium.Launch(playwright.BrowserTypeLaunchOptions{
		Headless: playwright.Bool(true),
	})
	if err != nil {
		return fmt.Errorf("could not launch chromium: %w", err)
	}
	s.browser = browser
	context, err := browser.NewContext(playwright.BrowserNewContextOptions{
		UserAgent: playwright.String(userAgent),
	})
	if err != nil {
		return fmt.Errorf("could not create browser context: %w", err)
	}
	s.page, err = context.NewPage()
	if err != nil {
		return fmt.Errorf("could not open page: %w", err)
	}
	return nil
}

// connectBrowserless asks Browserless's /unblock endpoint to load the site and
// solve its Cloudflare challenge, then attaches to that browser session and
// reuses its already-cleared page. Busy/timeout responses are retried with a
// growing backoff.
func (s *Scraper) connectBrowserless() error {
	var err error
	for attempt := 1; attempt <= browserlessUnblockAttempts; attempt++ {
		if attempt > 1 {
			backoff := time.Duration(attempt-1) * 20 * time.Second
			glog.Warningf("Retrying Browserless unblock in %s (attempt %d/%d) after: %s", backoff, attempt, browserlessUnblockAttempts, err)
			time.Sleep(backoff)
		}
		var retryable bool
		retryable, err = s.unblock()
		if err == nil || !retryable {
			return err
		}
	}
	return err
}

// unblock makes a single /unblock call and connects to the resulting session.
// It reports whether a failure is worth retrying.
func (s *Scraper) unblock() (retryable bool, err error) {
	glog.Infof("Unblocking %s via Browserless", baseURL)
	body, err := json.Marshal(map[string]interface{}{
		"url":               baseURL,
		"browserWSEndpoint": true,
		"ttl":               browserlessSessionTTL,
		"content":           false,
		"cookies":           false,
		"screenshot":        false,
	})
	if err != nil {
		return false, err
	}
	endpoint := browserlessURL + "/unblock?proxy=residential&timeout=90000&token=" + url.QueryEscape(s.browserlessToken)
	client := &http.Client{Timeout: 2 * time.Minute}
	resp, err := client.Post(endpoint, "application/json", bytes.NewReader(body))
	if err != nil {
		// Don't wrap err: *url.Error includes the request URL, token and all.
		return true, fmt.Errorf("browserless unblock request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		retryable := resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusRequestTimeout || resp.StatusCode >= 500
		return retryable, fmt.Errorf("browserless unblock returned status %d", resp.StatusCode)
	}
	var result struct {
		BrowserWSEndpoint string `json:"browserWSEndpoint"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return true, fmt.Errorf("could not decode browserless unblock response: %w", err)
	}
	if result.BrowserWSEndpoint == "" {
		return true, fmt.Errorf("browserless unblock response had no browserWSEndpoint")
	}

	browser, err := s.pw.Chromium.ConnectOverCDP(result.BrowserWSEndpoint + "?token=" + url.QueryEscape(s.browserlessToken))
	if err != nil {
		return true, fmt.Errorf("could not connect to browserless session: %w", err)
	}
	s.browser = browser
	contexts := browser.Contexts()
	if len(contexts) == 0 || len(contexts[0].Pages()) == 0 {
		browser.Close()
		s.browser = nil
		return true, fmt.Errorf("browserless session has no open page")
	}
	s.page = contexts[0].Pages()[0]
	s.sessionStart = time.Now()
	return false, nil
}

// reconnect replaces an expired or re-challenged Browserless session with a
// freshly unblocked one. It's a no-op for a local browser.
func (s *Scraper) reconnect() {
	if s.browserlessToken == "" {
		return
	}
	if s.browser != nil {
		s.browser.Close()
		s.browser = nil
	}
	s.page = nil
	if err := s.connectBrowserless(); err != nil {
		glog.Errorf("Could not reconnect to Browserless: %s", err)
	}
}

func (s *Scraper) Close() {
	if s.browser != nil {
		s.browser.Close()
	}
	if s.pw != nil {
		s.pw.Stop()
	}
}

// numDaysToFetch is how many days ahead (including today) each location is
// scraped for. Each location page takes a ?menuDate=yyyy-MM-dd query param
// that renders that date's hours and menu (the same content the site's own
// calendar widget navigates to when you click a date), so this isn't
// limited to what's visible in the calendar's current month view.
const numDaysToFetch = 7

// FetchAll scrapes every location in Locations for the next numDaysToFetch
// days and returns each location's dining hall info (name, group, and one
// DayHour per day scraped) plus every day's menus (one per location per
// meal period per day).
func (s *Scraper) FetchAll() (*pb.DiningHalls, []*pb.Menu, error) {
	dates := make([]time.Time, numDaysToFetch)
	for i := range dates {
		dates[i] = date.Now().AddDate(0, 0, i)
	}

	diningHalls := &pb.DiningHalls{}
	menus := []*pb.Menu{}

	for i, loc := range Locations {
		var name string
		var building *pb.DiningHall_Building
		dayHours := []*pb.DiningHall_DayHour{}

		for d, day := range dates {
			dateStr := date.FormatNoTime(day)
			formattedDate := day.Format("Monday, January 2, 2006")
			url := baseURL + loc.URLPath + "/?menuDate=" + dateStr

			doc, err := s.fetchDoc(url)
			if err != nil {
				glog.Errorf("Giving up on %s: %s", url, err)
			} else if pageName := strings.TrimSpace(strings.SplitN(doc.Find("title").First().Text(), "|", 2)[0]); pageName == "" {
				glog.Warningf("Could not determine location name for %s, skipping this date", url)
			} else {
				name = pageName
				if building == nil {
					building = parseBuilding(doc)
				}
				dayHours = append(dayHours, parseDayHours(doc, dateStr)...)
				menus = append(menus, parseMenus(doc, pageName, loc.Group, dateStr, formattedDate)...)
			}

			// Wait between requests so the fetch job behaves like a normal, slow
			// visitor rather than a scraping burst.
			if i < len(Locations)-1 || d < len(dates)-1 {
				time.Sleep(time.Duration(1500+rand.Intn(2000)) * time.Millisecond)
			}
		}

		if name == "" {
			glog.Warningf("Could not determine location name for %s, skipping", loc.URLPath)
			continue
		}
		diningHalls.DiningHalls = append(diningHalls.DiningHalls, &pb.DiningHall{
			Name:     name,
			Campus:   loc.Group,
			Building: building,
			DayHours: dayHours,
		})
	}

	return diningHalls, menus, nil
}

// maxFetchAttempts is how many times fetchDoc retries a single location
// before giving up on it for this run. The first navigation in a fresh
// browser context sometimes has to pay a Cloudflare challenge that can push
// it past the per-request timeout, especially on resource-constrained hosts;
// retrying keeps that one-off cost from silently dropping a location for the
// whole day.
const maxFetchAttempts = 3

// fetchDoc loads url and returns its rendered HTML as a parsed document,
// retrying transient failures (timeouts, non-200s, unparsable content). With
// Browserless, a failure usually means the session expired or Cloudflare
// challenged again, so each retry starts from a freshly unblocked session.
func (s *Scraper) fetchDoc(url string) (*goquery.Document, error) {
	var lastErr error
	for attempt := 1; attempt <= maxFetchAttempts; attempt++ {
		if attempt > 1 {
			glog.Warningf("Retrying %s (attempt %d/%d) after: %s", url, attempt, maxFetchAttempts, lastErr)
			time.Sleep(time.Duration(attempt) * time.Second)
			s.reconnect()
		} else if s.browserlessToken != "" && time.Since(s.sessionStart) > browserlessSessionMaxAge {
			glog.Infof("Rotating Browserless session after %s", time.Since(s.sessionStart).Round(time.Second))
			s.reconnect()
		}
		if s.page == nil {
			lastErr = fmt.Errorf("no browser page available")
			continue
		}
		page := s.page
		glog.Infof("Scraping %s", url)
		resp, err := page.Goto(url, playwright.PageGotoOptions{
			WaitUntil: playwright.WaitUntilStateDomcontentloaded,
			Timeout:   playwright.Float(30000),
		})
		if err != nil {
			lastErr = fmt.Errorf("failed to load: %w", err)
			continue
		}
		if resp.Status() != 200 {
			lastErr = fmt.Errorf("non-200 status %d", resp.Status())
			continue
		}
		html, err := page.Content()
		if err != nil {
			lastErr = fmt.Errorf("failed to read content: %w", err)
			continue
		}
		doc, err := goquery.NewDocumentFromReader(strings.NewReader(html))
		if err != nil {
			lastErr = fmt.Errorf("failed to parse html: %w", err)
			continue
		}
		return doc, nil
	}
	return nil, lastErr
}

// hoursTextReplacer normalizes the non-breaking spaces and non-breaking
// hyphen the site renders times with (e.g. "11:30 am ‑ 1:00 pm")
// into plain ASCII.
var hoursTextReplacer = strings.NewReplacer(" ", " ", "‑", "-")

// parseDayHours scrapes the "Today's Hours" box on a location page. The site
// only ever renders hours for the day it's currently showing, so this
// returns at most one DayHour (keyed by date) listing each serving
// period/time range found; a closed location has no calhours entries and
// yields none.
func parseDayHours(doc *goquery.Document, date string) []*pb.DiningHall_DayHour {
	hours := []string{}
	doc.Find(".hours-content .calhours > li").Each(func(_ int, li *goquery.Selection) {
		title := strings.TrimSpace(li.Find(".calhours-title").First().Text())
		times := strings.TrimSpace(hoursTextReplacer.Replace(li.Find(".calhours-times").First().Text()))
		if title == "" && times == "" {
			return
		}
		if times == "" {
			hours = append(hours, title)
		} else {
			hours = append(hours, title+": "+times)
		}
	})
	if len(hours) == 0 {
		return nil
	}
	return []*pb.DiningHall_DayHour{{Key: date, Hour: hours}}
}

// parseBuilding scrapes the address line from a location page's
// "location-details" box (e.g. "701 East University, Ann Arbor, MI, 48109").
// The site doesn't consistently punctuate this line (some locations omit the
// comma before the city, or before the zip), so this stores it as a single
// street1 value rather than risking a wrong street/city split; downstream
// consumers get the full address either way.
func parseBuilding(doc *goquery.Document) *pb.DiningHall_Building {
	address := strings.Join(strings.Fields(doc.Find("ul.location-details li.address").First().Text()), " ")
	if address == "" {
		return nil
	}
	return &pb.DiningHall_Building{
		Address: &pb.DiningHall_Building_Address{
			Street1: address,
		},
	}
}

// unlabeledMeal is the synthetic Meal value used for cafes and markets,
// whose pages (unlike dining halls) don't split their menu into named
// meal-period sections.
const unlabeledMeal = "Menu"

func parseMenus(doc *goquery.Document, diningHallName string, group string, date string, formattedDate string) []*pb.Menu {
	menus := []*pb.Menu{}

	h3s := doc.Find("#mdining-items > h3")
	if h3s.Length() > 0 {
		h3s.Each(func(_ int, h3 *goquery.Selection) {
			meal := strings.TrimSpace(h3.Find("a").First().Text())
			if meal == "" {
				return
			}
			categories := parseCategories(h3.Next())
			menus = append(menus, buildMenu(diningHallName, group, meal, date, formattedDate, categories))
		})
		return menus
	}

	// Cafes and markets don't render per-meal <h3> sections; when they have
	// a menu at all, it's a single unlabeled course list directly under
	// #mdining-items (24/7 kiosks like vending markets have neither).
	courseHijax := doc.Find("#mdining-items > .course-hijax").First()
	categories := parseCategories(courseHijax)
	if len(categories) > 0 {
		menus = append(menus, buildMenu(diningHallName, group, unlabeledMeal, date, formattedDate, categories))
		return menus
	}
	// TEMP DIAGNOSTIC (remove once we know why prod sees 0 cafe/market menus
	// despite this working reliably against live pages from a dev machine):
	// distinguishes "no #mdining-items at all" (24/7 kiosk, no menu page)
	// from "#mdining-items present but empty" (genuine no-menu-today) from
	// "course-hijax present but parsed 0 categories" (markup mismatch or a
	// JS-rendering race under WaitUntilStateDomcontentloaded).
	glog.Warningf(
		"parseMenus fallback got 0 categories for %s on %s: mdining-items=%v course-hijax=%v no-menu=%v",
		diningHallName, date,
		doc.Find("#mdining-items").Length() > 0,
		courseHijax.Length() > 0,
		doc.Find("#mdining-items .no-menu").Length() > 0,
	)
	return menus
}

func parseCategories(container *goquery.Selection) []*pb.Category {
	categories := []*pb.Category{}
	container.Find("ul.courses_wrapper > li").Each(func(_ int, catLi *goquery.Selection) {
		catName := strings.TrimSpace(catLi.ChildrenFiltered("h4").First().Text())
		menuItems := []*pb.MenuItem{}
		catLi.Find("ul.items > li").Each(func(_ int, itemLi *goquery.Selection) {
			// Cafe/market item names embed a nested .price span (e.g.
			// "Michigan Mocha  $5.50") that dining hall items don't have;
			// drop it and collapse the whitespace it leaves behind rather
			// than storing the price glued onto the name (there's no
			// dedicated price field to put it in instead).
			nameEl := itemLi.Find(".item-name").First().Clone()
			nameEl.Find(".price").Remove()
			itemName := strings.Join(strings.Fields(nameEl.Text()), " ")
			if itemName == "" {
				return
			}
			attribute, allergens := parseTraitsAndAllergens(itemLi)
			nutDiv := itemLi.NextFiltered("div.nutrition")
			if nutDiv.Length() == 0 {
				nutDiv = itemLi.Find("div.nutrition")
			}
			itemSizes := ParseNutritionInfo(nutDiv)
			menuItems = append(menuItems, &pb.MenuItem{
				Name:      itemName,
				Attribute: attribute,
				Allergens: allergens,
				ItemSizes: itemSizes,
			})
		})
		if catName != "" || len(menuItems) > 0 {
			categories = append(categories, &pb.Category{Name: catName, MenuItem: menuItems})
		}
	})
	return categories
}

func ParseNutritionInfo(nutritionDiv *goquery.Selection) []*pb.ItemSizes {
	if nutritionDiv == nil || nutritionDiv.Length() == 0 {
		return nil
	}

	if nutritionDiv.Find(".no-nutrition-facts").Length() > 0 {
		return nil
	}

	table := nutritionDiv.Find("table.nutrition-facts")
	if table.Length() == 0 {
		return nil
	}

	servingSizeText := strings.TrimSpace(table.Find(".serving-size").Text())
	servingSizeText = strings.TrimPrefix(servingSizeText, "Serving Size")
	servingSizeText = strings.TrimSpace(servingSizeText)

	nutritionalInfoList := []*pb.NutritionalInfo{}

	caloriesText := strings.TrimSpace(table.Find(".portion-calories").Text())
	caloriesText = strings.TrimPrefix(caloriesText, "Calories")
	caloriesText = strings.TrimSpace(caloriesText)
	if caloriesVal, err := strconv.Atoi(caloriesText); err == nil {
		nutritionalInfoList = append(nutritionalInfoList, &pb.NutritionalInfo{
			Name:  "Calories",
			Value: int32(caloriesVal),
			Units: "kcal",
			Type:  "CALORIES",
		})
	}

	table.Find("tr").Each(func(_ int, tr *goquery.Selection) {
		class, _ := tr.Attr("class")
		if strings.Contains(class, "nutrition-facts-header") ||
			strings.Contains(class, "serving-size") ||
			strings.Contains(class, "portion-calories") {
			return
		}

		tds := tr.Find("td")
		if tds.Length() < 1 {
			return
		}

		col1 := strings.TrimSpace(tds.Eq(0).Text())
		if col1 == "" || strings.HasPrefix(col1, "Amount Per Serving") || strings.HasPrefix(col1, "% Daily Value") {
			return
		}

		col2 := ""
		if tds.Length() >= 2 {
			col2 = strings.TrimSpace(tds.Eq(1).Text())
		}

		if strings.Contains(class, "micronutrient") {
			nutName := col1
			var dv int32 = 0
			if matches := percentDailyRegex.FindStringSubmatch(col2); len(matches) > 1 {
				if v, err := strconv.Atoi(matches[1]); err == nil {
					dv = int32(v)
				}
			}
			nutritionalInfoList = append(nutritionalInfoList, &pb.NutritionalInfo{
				Name:              nutName,
				PercentDailyValue: dv,
				Type:              "MICRONUTRIENT",
			})
			return
		}

		parts := strings.Fields(col1)
		if len(parts) == 0 {
			return
		}

		lastPart := parts[len(parts)-1]
		nutName := strings.Join(parts[:len(parts)-1], " ")

		var val int32 = 0
		units := ""
		if matches := valueUnitRegex.FindStringSubmatch(lastPart); len(matches) > 1 {
			if v, err := strconv.ParseFloat(matches[1], 64); err == nil {
				val = int32(v)
			}
			units = matches[2]
		} else {
			nutName = col1
		}

		var dv int32 = 0
		if matches := percentDailyRegex.FindStringSubmatch(col2); len(matches) > 1 {
			if v, err := strconv.Atoi(matches[1]); err == nil {
				dv = int32(v)
			}
		}

		nutType := "MACRONUTRIENT"
		if strings.Contains(class, "indent") {
			nutType = "SUB_NUTRIENT"
		}

		nutritionalInfoList = append(nutritionalInfoList, &pb.NutritionalInfo{
			Name:              nutName,
			Value:             val,
			Units:             units,
			PercentDailyValue: dv,
			Type:              nutType,
		})
	})

	if len(nutritionalInfoList) == 0 && servingSizeText == "" {
		return nil
	}

	return []*pb.ItemSizes{
		{
			ServingSize:     servingSizeText,
			NutritionalInfo: nutritionalInfoList,
		},
	}
}

func buildMenu(diningHallName, group, meal, date, formattedDate string, categories []*pb.Category) *pb.Menu {
	return &pb.Menu{
		Meal:             meal,
		Date:             date,
		FormattedDate:    formattedDate,
		HasCategories:    len(categories) > 0,
		Category:         categories,
		DiningHallName:   diningHallName,
		DiningHallMeal:   diningHallName + meal,
		DiningHallCampus: group,
	}
}

func parseTraitsAndAllergens(itemLi *goquery.Selection) (attributes []string, allergens []string) {
	class, _ := itemLi.Attr("class")
	for _, c := range strings.Fields(class) {
		switch {
		case strings.HasPrefix(c, "trait-"):
			attributes = append(attributes, strings.TrimPrefix(c, "trait-"))
		case strings.HasPrefix(c, "allergen-"):
			allergens = append(allergens, strings.TrimPrefix(c, "allergen-"))
		}
	}
	return attributes, allergens
}
