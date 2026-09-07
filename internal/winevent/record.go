// Package winevent reads Windows event-log records through the native
// Windows Event Log API (wevtapi.dll). Using the API directly — instead of
// shelling out to wevtutil/PowerShell — keeps the tool a single dependency-free
// binary and avoids console code-page corruption of non-ASCII (e.g. Japanese)
// reason strings, because EvtRender returns UTF-16 that we decode explicitly.
//
// This file holds the parts that do not touch the Windows API: the Record type
// the rest of the tool consumes, and the XML decoding that produces it. It
// carries no build tag on purpose, so packages that only need Record — detect,
// above all — stay testable on any platform.
package winevent

import (
	"encoding/xml"
	"fmt"
	"time"
)

// Record is a single parsed event-log entry.
type Record struct {
	Provider string            // System/Provider/@Name
	EventID  int               // System/EventID
	RecordID int64             // System/EventRecordID
	Time     time.Time         // System/TimeCreated/@SystemTime (UTC)
	Computer string            // System/Computer
	Data     map[string]string // EventData/Data Name -> value (positional entries keyed param1, param2, ...)
}

type xmlEvent struct {
	System struct {
		Provider struct {
			Name string `xml:"Name,attr"`
		} `xml:"Provider"`
		EventID     int `xml:"EventID"`
		TimeCreated struct {
			SystemTime string `xml:"SystemTime,attr"`
		} `xml:"TimeCreated"`
		EventRecordID int64  `xml:"EventRecordID"`
		Computer      string `xml:"Computer"`
	} `xml:"System"`
	EventData struct {
		Data []struct {
			Name  string `xml:"Name,attr"`
			Value string `xml:",chardata"`
		} `xml:"Data"`
	} `xml:"EventData"`
}

func parseRecord(s string) (Record, error) {
	var e xmlEvent
	if err := xml.Unmarshal([]byte(s), &e); err != nil {
		return Record{}, err
	}
	t, _ := time.Parse(time.RFC3339Nano, e.System.TimeCreated.SystemTime)
	rec := Record{
		Provider: e.System.Provider.Name,
		EventID:  e.System.EventID,
		RecordID: e.System.EventRecordID,
		Time:     t.UTC(),
		Computer: e.System.Computer,
		Data:     make(map[string]string, len(e.EventData.Data)),
	}
	for i, d := range e.EventData.Data {
		name := d.Name
		if name == "" {
			name = fmt.Sprintf("param%d", i+1)
		}
		rec.Data[name] = d.Value
	}
	return rec, nil
}
