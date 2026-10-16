package main

// Dreamcast IP.BIN header, read from the system area of the LD data track (LBA 0).
// Layout (offsets into the first 256 bytes):
//   0x00 "SEGA SEGAKATANA "   0x20 device info ("9903 GD-ROM1/1")
//   0x30 area symbols (J/U/E) 0x40 product number (10)  0x4A version ("V1.004")
//   0x50 release date (8)     0x80 software title

import (
	"fmt"
	"regexp"
	"strings"
)

type IPBin struct {
	Product  string // as on disc, e.g. "T20106M" or "MK-51000"
	Version  string // without the leading V, e.g. "1.004"
	Areas    string // e.g. "J", "JUE"
	Date     string
	Title    string
	Disc, Of int // from "GD-ROM1/2"
}

var reDiscNum = regexp.MustCompile(`GD-ROM\s*(\d+)\s*/\s*(\d+)`)

func ParseIPBin(sector []byte) (*IPBin, error) {
	if len(sector) < 256 || string(sector[0:15]) != "SEGA SEGAKATANA" {
		return nil, fmt.Errorf("no SEGA SEGAKATANA signature")
	}
	field := func(a, b int) string { return strings.Trim(string(sector[a:b]), " \x00") }
	ip := &IPBin{
		Product: field(0x40, 0x4A),
		Version: strings.TrimPrefix(field(0x4A, 0x50), "V"),
		Date:    field(0x50, 0x58),
		Title:   field(0x80, 0x100),
		Disc:    1, Of: 1,
	}
	for _, c := range field(0x30, 0x38) {
		if c != ' ' {
			ip.Areas += string(c)
		}
	}
	if m := reDiscNum.FindStringSubmatch(field(0x20, 0x30)); m != nil {
		fmt.Sscan(m[1], &ip.Disc)
		fmt.Sscan(m[2], &ip.Of)
	}
	return ip, nil
}

// Region guesses a redump region from the area symbols (only used as a fallback
// name when the disc is not in the DAT).
func (ip *IPBin) Region() string {
	switch ip.Areas {
	case "J":
		return "Japan"
	case "U":
		return "USA"
	case "E":
		return "Europe"
	}
	return "Japan"
}
