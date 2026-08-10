package main

import (
	"flag"
	"sync"

	"github.com/MichiganDiningAPI/api/mdining/mdiningscraper"
	dc "github.com/MichiganDiningAPI/db/dynamoclient"
	"github.com/MichiganDiningAPI/internal/processing/mdiningprocessing"
	util "github.com/MichiganDiningAPI/internal/util/containers"
	"github.com/golang/glog"
	"github.com/golang/protobuf/proto"
)

func main() {
	flag.Parse()

	scraper, err := mdiningscraper.New()
	if err != nil {
		glog.Fatalf("Failed to start scraper %s", err)
	}
	defer scraper.Close()

	dynamoclient := dc.New()
	dynamoclient.CreateTablesIfNotExists()

	diningHalls, menus, err := scraper.FetchAll()
	if err != nil {
		glog.Fatalf("Failed to scrape dining halls and menus %s", err)
	}

	diningHallsList := util.AsSliceType(diningHalls.DiningHalls, []proto.Message{}).([]proto.Message)
	dynamoclient.PutProtoBatch(&dc.DiningHallsTableName, diningHallsList)

	menusProtoSlice := util.AsSliceType(menus, []proto.Message{}).([]proto.Message)
	glog.Infof("Menus count: %d", len(menusProtoSlice))

	wg := sync.WaitGroup{}
	wg.Add(1)
	go func() {
		dynamoclient.PutProtoBatch(&dc.MenuTableName, menusProtoSlice)
		wg.Done()
	}()
	foodsSlice, err := mdiningprocessing.MenusToFoods(&menusProtoSlice)
	if err != nil {
		glog.Warningf("Could not convert menus to foods %s", err)
	} else {
		wg.Add(1)
		go func() {
			dynamoclient.PutProtoBatch(&dc.FoodTableName, foodsSlice)
			wg.Done()
		}()
	}
	wg.Wait()
}
