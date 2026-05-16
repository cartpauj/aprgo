// trailcheck: ad-hoc diagnostic. Walks the packets table for the last N
// minutes, parses each info field, and prints positions that look bogus —
// out of range, on null island, or that imply teleports between consecutive
// frames from the same source. Single-shot, not wired into the running
// server; build with:  go build -o trailcheck ./cmd/trailcheck
// and run on the host with the live DB.
package main

import (
	"database/sql"
	"flag"
	"fmt"
	"log"
	"math"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"aprgo/internal/aprs"
	"aprgo/internal/ax25"
)

type row struct {
	Time   time.Time
	Source string
	Dest   string
	Path   string
	Info   string
	Lat    float64
	Lon    float64
	Reason string
}

func main() {
	dbPath := flag.String("db", "/var/lib/aprgo/db.sqlite", "path to aprgo DB")
	mins := flag.Int("minutes", 1440, "lookback window")
	flag.Parse()

	db, err := sql.Open("sqlite", *dbPath+"?_pragma=journal_mode(WAL)")
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	since := time.Now().Add(-time.Duration(*mins) * time.Minute).Unix()
	rs, err := db.Query(`SELECT ts, source, COALESCE(dest,''), COALESCE(path,''), COALESCE(info,'')
		FROM packets WHERE ts >= ? ORDER BY ts ASC`, since)
	if err != nil {
		log.Fatal(err)
	}
	defer rs.Close()

	type point struct {
		t        time.Time
		lat, lon float64
		info     string
	}
	bySrc := map[string][]point{}

	var suspects []row
	add := func(r row) { suspects = append(suspects, r) }

	for rs.Next() {
		var ts int64
		var r row
		if err := rs.Scan(&ts, &r.Source, &r.Dest, &r.Path, &r.Info); err != nil {
			log.Fatal(err)
		}
		r.Time = time.Unix(ts, 0)
		fr := ax25.Frame{Src: r.Source, Dest: r.Dest, Info: []byte(r.Info), RxAt: r.Time}
		if r.Path != "" {
			fr.Path = strings.Split(r.Path, ",")
		}
		p := aprs.Parse(fr)
		if p.Decoded.Lat == nil || p.Decoded.Lon == nil {
			continue
		}
		r.Lat, r.Lon = *p.Decoded.Lat, *p.Decoded.Lon

		// Out of range
		if r.Lat < -90 || r.Lat > 90 || r.Lon < -180 || r.Lon > 180 {
			r.Reason = "out of range"
			add(r)
			continue
		}
		// Null island
		if math.Abs(r.Lat) < 0.5 && math.Abs(r.Lon) < 0.5 {
			r.Reason = "null island (~0,0)"
			add(r)
			continue
		}
		// Teleport vs. last point from this source
		last := bySrc[r.Source]
		if n := len(last); n > 0 {
			prev := last[n-1]
			dLat := math.Abs(r.Lat - prev.lat)
			dLon := math.Abs(r.Lon - prev.lon)
			distKm := (dLat + dLon) * 111.0
			elapsedHr := r.Time.Sub(prev.t).Hours()
			if elapsedHr > 0 && distKm/elapsedHr > 800 {
				r.Reason = fmt.Sprintf("teleport %.0fkm in %.0fmin (%.0f km/h) from prev=%.4f,%.4f",
					distKm, elapsedHr*60, distKm/elapsedHr, prev.lat, prev.lon)
				add(r)
				continue
			}
		}
		bySrc[r.Source] = append(bySrc[r.Source], point{t: r.Time, lat: r.Lat, lon: r.Lon, info: r.Info})
	}

	// Group suspects by source for readability, newest first.
	sort.Slice(suspects, func(i, j int) bool { return suspects[i].Time.After(suspects[j].Time) })
	fmt.Printf("Window: last %d minutes. %d suspicious positions found.\n\n", *mins, len(suspects))
	for _, s := range suspects {
		fmt.Printf("[%s] %-9s  %s  (%.5f, %.5f)\n", s.Time.Format("01-02 15:04:05"), s.Source, s.Reason, s.Lat, s.Lon)
		fmt.Printf("              info: %q\n", s.Info)
	}
	if len(suspects) == 0 {
		fmt.Println("(none — every parsed position looks reasonable)")
	}
}
