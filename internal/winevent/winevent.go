//go:build windows

// Package winevent reads Windows event-log records through the native
// Windows Event Log API (wevtapi.dll). Using the API directly — instead of
// shelling out to wevtutil/PowerShell — keeps the tool a single dependency-free
// binary and avoids console code-page corruption of non-ASCII (e.g. Japanese)
// reason strings, because EvtRender returns UTF-16 that we decode explicitly.
package winevent

import (
	"encoding/xml"
	"fmt"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
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

const (
	evtQueryChannelPath      = 0x1
	evtQueryReverseDirection = 0x200
	evtRenderEventXML        = 1

	errorNoMoreItems = 259
)

var (
	// NewLazySystemDLL resolves the name against %SystemRoot%\System32 only.
	// syscall.NewLazyDLL would follow the default search order, which looks in
	// the directory of the executable first — and this binary runs as SYSTEM, so
	// a wevtapi.dll dropped next to it would be loaded with those privileges.
	modwevtapi    = windows.NewLazySystemDLL("wevtapi.dll")
	procEvtQuery  = modwevtapi.NewProc("EvtQuery")
	procEvtNext   = modwevtapi.NewProc("EvtNext")
	procEvtRender = modwevtapi.NewProc("EvtRender")
	procEvtClose  = modwevtapi.NewProc("EvtClose")
)

// Query runs an XPath query against a channel (e.g. "System") and returns up to
// limit rendered records ordered newest-first.
func Query(channel, xpath string, limit int) ([]Record, error) {
	chPtr, err := syscall.UTF16PtrFromString(channel)
	if err != nil {
		return nil, err
	}
	qPtr, err := syscall.UTF16PtrFromString(xpath)
	if err != nil {
		return nil, err
	}

	handle, _, callErr := procEvtQuery.Call(
		0, // local session
		uintptr(unsafe.Pointer(chPtr)),
		uintptr(unsafe.Pointer(qPtr)),
		uintptr(evtQueryChannelPath|evtQueryReverseDirection),
	)
	if handle == 0 {
		return nil, fmt.Errorf("EvtQuery(%q): %w", channel, callErr)
	}
	defer procEvtClose.Call(handle)

	records := make([]Record, 0, limit)
	const batch = 16
	var firstErr error
	for len(records) < limit {
		// Never request more than we still want, so the result can't overshoot
		// limit (and we never render handles we'd discard).
		req := uint32(batch)
		if remaining := limit - len(records); remaining < batch {
			req = uint32(remaining)
		}
		var events [batch]uintptr
		var returned uint32
		ok, _, e := procEvtNext.Call(
			handle,
			uintptr(req),
			uintptr(unsafe.Pointer(&events[0])),
			10000, // timeout ms (ignored for query result sets)
			0,
			uintptr(unsafe.Pointer(&returned)),
		)
		if ok == 0 {
			if errno, isErrno := e.(syscall.Errno); isErrno && uintptr(errno) != errorNoMoreItems && firstErr == nil {
				firstErr = fmt.Errorf("EvtNext: %w", e)
			}
			break
		}
		for i := 0; i < int(returned); i++ {
			xmlStr, rerr := renderXML(events[i])
			procEvtClose.Call(events[i])
			if rerr != nil {
				if firstErr == nil {
					firstErr = rerr
				}
				continue
			}
			rec, perr := parseRecord(xmlStr)
			if perr != nil {
				if firstErr == nil {
					firstErr = perr
				}
				continue
			}
			records = append(records, rec)
		}
	}
	// Only surface a render/parse/next error when it left us with nothing;
	// otherwise a single malformed event shouldn't fail the whole query.
	if len(records) == 0 && firstErr != nil {
		return nil, firstErr
	}
	return records, nil
}

func renderXML(event uintptr) (string, error) {
	var used, propCount uint32
	// First call with a nil buffer to learn the required size (bytes).
	procEvtRender.Call(0, event, evtRenderEventXML, 0, 0,
		uintptr(unsafe.Pointer(&used)), uintptr(unsafe.Pointer(&propCount)))
	if used == 0 {
		return "", fmt.Errorf("EvtRender returned zero size")
	}
	buf := make([]uint16, used/2+1)
	ok, _, e := procEvtRender.Call(0, event, evtRenderEventXML,
		uintptr(used), uintptr(unsafe.Pointer(&buf[0])),
		uintptr(unsafe.Pointer(&used)), uintptr(unsafe.Pointer(&propCount)))
	if ok == 0 {
		return "", fmt.Errorf("EvtRender: %w", e)
	}
	return syscall.UTF16ToString(buf), nil
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
