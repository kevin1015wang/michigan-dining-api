// Package mdiningscraper fetches menu data by rendering and parsing the public
// dining.umich.edu location pages. UMich retired the JSON APIs the old clients
// (mdiningclient, mdiningclient2) used to hit, and the site is now behind a
// Cloudflare bot check that a plain HTTP client can't pass, so this drives a
// real headless browser (which the challenge is designed to let through) and
// scrapes the server-rendered menu markup instead.
package mdiningscraper

import (
	"fmt"
	"math/rand"
	"strings"
	"time"

	"github.com/MichiganDiningAPI/internal/util/date"
	pb "github.com/MichiganDiningAPI/proto/mdining"
	"github.com/PuerkitoBio/goquery"
	"github.com/golang/glog"
	"github.com/mxschmitt/playwright-go"
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

// Scraper drives a headless browser to render and scrape dining.umich.edu pages.
type Scraper struct {
	pw      *playwright.Playwright
	browser playwright.Browser
}

// New starts a headless Chromium instance. Call Close when done.
// Requires the Playwright browser binaries to be installed; if missing, run
// `go run github.com/mxschmitt/playwright-go/cmd/playwright install --with-deps chromium`.
func New() (*Scraper, error) {
	pw, err := playwright.Run()
	if err != nil {
		return nil, fmt.Errorf("could not start playwright: %w", err)
	}
	browser, err := pw.Chromium.Launch(playwright.BrowserTypeLaunchOptions{
		Headless: playwright.Bool(true),
	})
	if err != nil {
		pw.Stop()
		return nil, fmt.Errorf("could not launch chromium: %w", err)
	}
	return &Scraper{pw: pw, browser: browser}, nil
}

func (s *Scraper) Close() {
	if s.browser != nil {
		s.browser.Close()
	}
	if s.pw != nil {
		s.pw.Stop()
	}
}

// FetchAll scrapes every location in Locations and returns today's dining
// halls (name + group) and today's menus (one per location per meal period).
func (s *Scraper) FetchAll() (*pb.DiningHalls, []*pb.Menu, error) {
	context, err := s.browser.NewContext(playwright.BrowserNewContextOptions{
		UserAgent: playwright.String(userAgent),
	})
	if err != nil {
		return nil, nil, fmt.Errorf("could not create browser context: %w", err)
	}
	defer context.Close()
	page, err := context.NewPage()
	if err != nil {
		return nil, nil, fmt.Errorf("could not open page: %w", err)
	}

	today := date.FormatNoTime(date.Now())
	formattedToday := date.Now().Format("Monday, January 2, 2006")

	diningHalls := &pb.DiningHalls{}
	menus := []*pb.Menu{}

	for i, loc := range Locations {
		url := baseURL + loc.URLPath + "/"
		glog.Infof("Scraping %s", url)
		resp, err := page.Goto(url, playwright.PageGotoOptions{
			WaitUntil: playwright.WaitUntilStateDomcontentloaded,
			Timeout:   playwright.Float(30000),
		})
		if err != nil {
			glog.Warningf("Failed to load %s: %s", url, err)
			continue
		}
		if resp.Status() != 200 {
			glog.Warningf("Non-200 status %d for %s", resp.Status(), url)
			continue
		}
		html, err := page.Content()
		if err != nil {
			glog.Warningf("Failed to read content for %s: %s", url, err)
			continue
		}
		doc, err := goquery.NewDocumentFromReader(strings.NewReader(html))
		if err != nil {
			glog.Warningf("Failed to parse html for %s: %s", url, err)
			continue
		}

		name := strings.TrimSpace(strings.SplitN(doc.Find("title").First().Text(), "|", 2)[0])
		if name == "" {
			glog.Warningf("Could not determine location name for %s, skipping", url)
			continue
		}

		diningHalls.DiningHalls = append(diningHalls.DiningHalls, &pb.DiningHall{
			Name:   name,
			Campus: loc.Group,
		})

		locationMenus := parseMenus(doc, name, loc.Group, today, formattedToday)
		menus = append(menus, locationMenus...)

		// Wait between requests so the fetch job behaves like a normal, slow
		// visitor rather than a scraping burst.
		if i < len(Locations)-1 {
			time.Sleep(time.Duration(1500+rand.Intn(2000)) * time.Millisecond)
		}
	}

	return diningHalls, menus, nil
}

func parseMenus(doc *goquery.Document, diningHallName string, group string, date string, formattedDate string) []*pb.Menu {
	menus := []*pb.Menu{}
	doc.Find("#mdining-items > h3").Each(func(_ int, h3 *goquery.Selection) {
		meal := strings.TrimSpace(h3.Find("a").First().Text())
		if meal == "" {
			return
		}

		categories := []*pb.Category{}
		h3.Next().Find("ul.courses_wrapper > li").Each(func(_ int, catLi *goquery.Selection) {
			catName := strings.TrimSpace(catLi.ChildrenFiltered("h4").First().Text())
			menuItems := []*pb.MenuItem{}
			catLi.Find("ul.items > li").Each(func(_ int, itemLi *goquery.Selection) {
				itemName := strings.TrimSpace(itemLi.Find(".item-name").First().Text())
				if itemName == "" {
					return
				}
				attribute, allergens := parseTraitsAndAllergens(itemLi)
				menuItems = append(menuItems, &pb.MenuItem{
					Name:      itemName,
					Attribute: attribute,
					Allergens: allergens,
				})
			})
			if catName != "" || len(menuItems) > 0 {
				categories = append(categories, &pb.Category{Name: catName, MenuItem: menuItems})
			}
		})

		menus = append(menus, &pb.Menu{
			Meal:             meal,
			Date:             date,
			FormattedDate:    formattedDate,
			HasCategories:    len(categories) > 0,
			Category:         categories,
			DiningHallName:   diningHallName,
			DiningHallMeal:   diningHallName + meal,
			DiningHallCampus: group,
		})
	})
	return menus
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
