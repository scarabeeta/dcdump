package main

// Disc identification: serial from
// --serial, else from IP.BIN (auto) or a prompt (manual); DAT lookup; a provisional
// name when several entries share the serial (settled by hash after the dump);
// "Unknown (...)" + identify-by-hash in auto mode when the disc is not in the DAT.

import (
	"fmt"
	"regexp"
	"strings"
)

var regions = []string{"Japan", "USA", "Europe", "Other..."}

type DiscInfo struct {
	SerialInput    string
	DatEntry       *DatEntry
	Candidates     []*DatEntry
	DatName        string
	Basename       string
	Region         string
	IdentifyByHash bool
}

var (
	reTrailingParen = regexp.MustCompile(`\(([^)]+)\)\s*$`)
	reDiscTag       = regexp.MustCompile(`\(Disc (\d+)\)`)
	reUnsafe        = regexp.MustCompile(`[^A-Za-z0-9 ._-]`)
)

// narrow orders candidates so that entries matching the IP.BIN version and disc
// number come first; the first one becomes the provisional pick.
func narrow(c []*DatEntry, ip *IPBin) []*DatEntry {
	if ip == nil || len(c) < 2 {
		return c
	}
	score := func(e *DatEntry) int {
		s := 0
		if ip.Version != "" && e.Version == ip.Version {
			s += 2
		}
		m := reDiscTag.FindStringSubmatch(e.Name)
		if (m == nil && ip.Of == 1) || (m != nil && m[1] == fmt.Sprint(ip.Disc)) {
			s++
		}
		return s
	}
	out := append([]*DatEntry(nil), c...)
	for i := 1; i < len(out); i++ { // stable insertion sort by score, descending
		for j := i; j > 0 && score(out[j]) > score(out[j-1]); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

func IdentifyDisc(auto bool, ip *IPBin, db *DAT, serialArg string) *DiscInfo {
	info := &DiscInfo{}
	detected := ""
	if ip != nil {
		detected = ip.Product
		fmt.Printf("  Detected serial from IP.BIN: %s  (version %s, disc %d/%d)\n", ip.Product, ip.Version, ip.Disc, ip.Of)
	}
	switch {
	case serialArg != "":
		info.SerialInput = serialArg
		fmt.Printf("  [--serial] Serial: %s\n", serialArg)
	case auto:
		info.SerialInput = detected
		if detected != "" {
			fmt.Printf("  [auto] Serial: %s\n", detected)
		}
	default:
		info.SerialInput = ask("Serial (e.g. T-20106M, leave blank to skip)", detected)
	}

	datRegion := ""
	if info.SerialInput != "" {
		info.Candidates = narrow(db.Lookup(info.SerialInput), ip)
		if len(info.Candidates) > 0 {
			info.DatEntry = info.Candidates[0]
			info.DatName = info.DatEntry.Name
			if m := reTrailingParen.FindStringSubmatchIndex(info.DatName); m != nil {
				datRegion = info.DatName[m[2]:m[3]]
				info.Basename = strings.TrimSpace(info.DatName[:m[0]])
			} else {
				info.Basename = info.DatName
			}
			if len(info.Candidates) == 1 {
				fmt.Printf("  Found: %s\n", info.DatName)
			} else {
				fmt.Printf("  Found %d DAT entries for this serial:\n", len(info.Candidates))
				for _, c := range info.Candidates {
					fmt.Printf("    - %s  (version %s)\n", c.Name, c.Version)
				}
				fmt.Printf("  Provisional name: %s\n", info.DatName)
				fmt.Println("  (the entry whose track SHA-1s match the dump decides the final name)")
			}
		} else {
			fmt.Printf("  Serial %s not found in DAT.\n", NormalizeSerial(info.SerialInput))
		}
	}
	if datRegion == "" && ip != nil {
		datRegion = ip.Region()
	}
	if datRegion == "" {
		datRegion = "Japan"
	}
	fmt.Println()

	if auto && len(info.Candidates) == 0 {
		label := ""
		if ip != nil {
			label = strings.TrimSpace(reUnsafe.ReplaceAllString(ip.Title, ""))
		}
		if label == "" {
			label = NormalizeSerial(info.SerialInput)
		}
		if label == "" {
			label = "no label"
		}
		prov := fmt.Sprintf("Unknown (%s)", label)
		fmt.Printf("  [auto] Not identified yet — dumping as %q;\n", prov)
		fmt.Println("         the dump is matched against the whole DAT by track SHA-1 afterwards.")
		fmt.Println()
		info.DatName, info.Basename, info.IdentifyByHash = prov, prov, true
		return info
	}

	if auto && info.Basename != "" {
		fmt.Printf("  [auto] Game name: %s\n", info.Basename)
	} else {
		for {
			if n := ask("Game name (basename)", info.Basename); n != "" {
				info.Basename = n
				break
			}
			fmt.Println("  Name cannot be empty.")
		}
	}
	if auto {
		info.Region = datRegion
		fmt.Printf("  [auto] Region: %s\n", info.Region)
	} else {
		def := len(regions) - 1
		for i, r := range regions {
			if r == datRegion {
				def = i
			}
		}
		fmt.Println("Region:")
		i := choose("Select region", regions, def)
		if i == len(regions)-1 {
			info.Region = ask("Region name", datRegion)
		} else {
			info.Region = regions[i]
		}
	}
	fmt.Printf("  Region: %s\n", info.Region)
	fmt.Println()
	return info
}

func (info *DiscInfo) FullName() string {
	if info.DatName != "" {
		return info.DatName
	}
	return fmt.Sprintf("%s (%s)", info.Basename, info.Region)
}
