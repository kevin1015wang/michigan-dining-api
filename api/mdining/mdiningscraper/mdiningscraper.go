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

	dates := make([]time.Time, numDaysToFetch)
	for i := range dates {
		dates[i] = date.Now().AddDate(0, 0, i)
	}

	diningHalls := &pb.DiningHalls{}
	menus := []*pb.Menu{}

	for i, loc := range Locations {
		var name string
		dayHours := []*pb.DiningHall_DayHour{}

		for d, day := range dates {
			dateStr := date.FormatNoTime(day)
			formattedDate := day.Format("Monday, January 2, 2006")
			url := baseURL + loc.URLPath + "/?menuDate=" + dateStr

			doc, err := fetchDoc(page, url)
			if err != nil {
				glog.Errorf("Giving up on %s: %s", url, err)
			} else if pageName := strings.TrimSpace(strings.SplitN(doc.Find("title").First().Text(), "|", 2)[0]); pageName == "" {
				glog.Warningf("Could not determine location name for %s, skipping this date", url)
			} else {
				name = pageName
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
// retrying transient failures (timeouts, non-200s, unparsable content).
func fetchDoc(page playwright.Page, url string) (*goquery.Document, error) {
	var lastErr error
	for attempt := 1; attempt <= maxFetchAttempts; attempt++ {
		if attempt > 1 {
			glog.Warningf("Retrying %s (attempt %d/%d) after: %s", url, attempt, maxFetchAttempts, lastErr)
			time.Sleep(time.Duration(attempt) * time.Second)
		}
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
