package main

// --native: read the disc in a real Dreamcast over the network and compare it
// with redump, as a hash source that shares nothing with the PC drive path
// (different drive, different read mechanism, no redumper/dreamdump).
//
// The Dreamcast runs Linux with a gdrom driver that can read raw 2352-byte sectors
// and a "gdserve" service on TCP port 9200, which answers one request per
// connection in plain text:
//
//	info              kernel, drive mode page, raw-read settings
//	toc               "ld|hd track N ctrl C fad F" lines plus the lead-outs
//	read FAD COUNT    COUNT raw 2352-byte sectors from absolute frame FAD
//	cooked LBA COUNT  COUNT 2048-byte sectors, error-corrected by the drive
//
// The drive returns data sectors descrambled and aligned but not error-corrected,
// has no subchannel or C2 output and is not a drive redump accepts, so the
// result is a verification report, never a submission.

import (
	"bufio"
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// nativeHostEnv names the console, so that --native can be used without an argument.
const nativeHostEnv = "DCDUMP_NATIVE_HOST"

const (
	nativePort     = "9200"
	ldStartFAD     = 150
	hdStartFAD     = hdStartLBA + 150
	nativeRetries  = 5
	transitionSpan = 6 // data frames after audio that may need the read-through
	nativeIOTimout = 40 * time.Second
)

type nativeDC struct{ addr string }

func (dc *nativeDC) request(cmd string) (net.Conn, error) {
	c, err := net.DialTimeout("tcp", dc.addr, 5*time.Second)
	if err != nil {
		return nil, err
	}
	c.SetDeadline(time.Now().Add(nativeIOTimout))
	if _, err := io.WriteString(c, cmd+"\n"); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

func (dc *nativeDC) text(cmd string) (string, error) {
	c, err := dc.request(cmd)
	if err != nil {
		return "", err
	}
	defer c.Close()
	b, err := io.ReadAll(c)
	return string(b), err
}

// stream opens a read of count units; the caller reads until EOF. A short
// stream means the Dreamcast hit an unreadable sector at that point.
type nativeStream struct {
	c net.Conn
	r *bufio.Reader
}

func (dc *nativeDC) stream(cmd string, first, count int) (*nativeStream, error) {
	c, err := dc.request(fmt.Sprintf("%s %d %d", cmd, first, count))
	if err != nil {
		return nil, err
	}
	return &nativeStream{c, bufio.NewReaderSize(c, 1<<18)}, nil
}

func (s *nativeStream) Read(p []byte) (int, error) {
	s.c.SetDeadline(time.Now().Add(nativeIOTimout))
	return s.r.Read(p)
}
func (s *nativeStream) Close() { s.c.Close() }

// readRaw returns exactly count raw sectors, or fewer if the stream ended early.
func (dc *nativeDC) readRaw(fad, count int) ([]byte, error) {
	s, err := dc.stream("read", fad, count)
	if err != nil {
		return nil, err
	}
	defer s.Close()
	buf := make([]byte, count*sectorRaw)
	n, err := io.ReadFull(s, buf)
	if err == io.ErrUnexpectedEOF || err == io.EOF {
		err = nil
	}
	return buf[:n/sectorRaw*sectorRaw], err
}

type nativeTOCTrack struct {
	Num   int
	Audio bool
	HD    bool
	FAD   int
}

type nativeTOC struct {
	Tracks               []nativeTOCTrack
	LDLeadout, HDLeadout int // FAD; 0 if that area's TOC is missing
}

var reRawSize = regexp.MustCompile(`(?m)^raw_sel \d+ raw_size 2352$`)

var reTOCLine = regexp.MustCompile(`^(ld|hd) (?:track +(\d+)|leadout) +ctrl ([0-9a-f]) fad +(\d+)$`)

func parseNativeTOC(text string) *nativeTOC {
	t := &nativeTOC{}
	for _, l := range strings.Split(text, "\n") {
		m := reTOCLine.FindStringSubmatch(strings.TrimSpace(l))
		if m == nil {
			continue
		}
		fad, _ := strconv.Atoi(m[4])
		hd := m[1] == "hd"
		if m[2] == "" {
			if hd {
				t.HDLeadout = fad
			} else {
				t.LDLeadout = fad
			}
			continue
		}
		n, _ := strconv.Atoi(m[2])
		ctrl, _ := strconv.ParseInt(m[3], 16, 8)
		t.Tracks = append(t.Tracks, nativeTOCTrack{Num: n, Audio: ctrl&4 == 0, HD: hd, FAD: fad})
	}
	return t
}

// nativeTrack is one output file: Sectors raw sectors starting at frame Start.
// Lead counts the frames at the head of a data track's file that are still
// recorded as audio: the gap a preceding audio track leaves before the data
// pregap, which begins 150 frames ahead of the TOC address.
type nativeTrack struct {
	nativeTOCTrack
	Start, Sectors int
	Lead           int
}

func setLeads(tr []nativeTrack) {
	for i := 1; i < len(tr); i++ {
		t := &tr[i]
		if !t.Audio && tr[i-1].Audio && tr[i-1].HD == t.HD {
			t.Lead = max(0, min(t.FAD-150-t.Start, t.Sectors))
		}
	}
}

// nativeLayout decides which frames belong to which track file. redump track
// files are contiguous within an area, so for a disc found in the DAT the file
// sizes give the boundaries exactly; they must add up to the area the TOC
// describes. Without a DAT entry the usual pregap rules are applied (an audio
// track owns the 150 frames before its TOC address, a data track after audio
// 225); discs checked against the DAT follow these rules.
func nativeLayout(toc *nativeTOC, entry *DatEntry) ([]nativeTrack, string, error) {
	if len(toc.Tracks) == 0 || toc.LDLeadout == 0 || toc.HDLeadout == 0 {
		return nil, "", fmt.Errorf("incomplete TOC")
	}
	out := make([]nativeTrack, len(toc.Tracks))
	for i, t := range toc.Tracks {
		out[i].nativeTOCTrack = t
	}
	areaEnd := func(hd bool) int {
		if hd {
			return toc.HDLeadout
		}
		return toc.LDLeadout
	}
	areaStart := func(hd bool) int {
		if hd {
			return hdStartFAD
		}
		return ldStartFAD
	}
	byRule := func() {
		for i := range out {
			t := &out[i]
			switch {
			case i == 0 || out[i-1].HD != t.HD:
				t.Start = areaStart(t.HD)
			case t.Audio:
				t.Start = t.FAD - 150
			case out[i-1].Audio:
				t.Start = t.FAD - 225
			default:
				t.Start = t.FAD
			}
		}
		for i := range out {
			end := areaEnd(out[i].HD)
			if i+1 < len(out) && out[i+1].HD == out[i].HD {
				end = out[i+1].Start
			}
			out[i].Sectors = end - out[i].Start
		}
	}
	if entry == nil {
		byRule()
		setLeads(out)
		return out, "track boundaries from the TOC and pregap rules (disc not in the DAT)", nil
	}
	size := map[int]int64{}
	for _, d := range entry.Tracks {
		if m := reTrackNo.FindStringSubmatch(d.Name); m != nil && strings.HasSuffix(strings.ToLower(d.Name), ".bin") {
			n, _ := strconv.Atoi(m[1])
			size[n] = d.Size
		}
	}
	pos := 0
	for i := range out {
		t := &out[i]
		sz, ok := size[t.Num]
		if !ok || sz%sectorRaw != 0 {
			return nil, "", fmt.Errorf("DAT entry has no usable size for track %d", t.Num)
		}
		if i == 0 || out[i-1].HD != t.HD {
			pos = areaStart(t.HD)
		}
		t.Start, t.Sectors = pos, int(sz/sectorRaw)
		pos += t.Sectors
		if i+1 == len(out) || out[i+1].HD != t.HD {
			if pos != areaEnd(t.HD) {
				return nil, "", fmt.Errorf("DAT track sizes end at frame %d but the TOC lead-out is at %d: different disc layout", pos, areaEnd(t.HD))
			}
		}
		if t.Start > t.FAD {
			return nil, "", fmt.Errorf("track %d: DAT sizes put its file after its TOC address", t.Num)
		}
	}
	if len(size) != len(out) {
		return nil, "", fmt.Errorf("DAT entry has %d tracks, the TOC %d", len(size), len(out))
	}
	setLeads(out)
	return out, "track boundaries from the DAT track sizes, consistent with the TOC", nil
}

// audioOffset measures where the audio stream sits relative to data
// addressing, in bytes: the drive addresses data sectors by their header but
// audio by subcode, so reading the frames right after a data track as audio
// still shows that track's last sector (scrambled). The distance of its sync
// from where its address says it belongs is the combined write + read offset.
func audioOffset(dc *nativeDC, firstAudioFAD int) (int, error) {
	buf, err := dc.readRaw(firstAudioFAD, 4)
	if err != nil {
		return 0, err
	}
	for i := 0; i+16 <= len(buf); i++ {
		if buf[i] != 0 || buf[i+1] != 0xFF {
			continue
		}
		if fad := ScrambledHeaderFAD(buf[i:]); fad >= 0 && fad < firstAudioFAD && firstAudioFAD-fad <= 3 {
			return i + (firstAudioFAD-fad)*sectorRaw, nil
		}
	}
	return 0, fmt.Errorf("no data sector found in the audio stream after frame %d", firstAudioFAD-1)
}

type nativeStats struct {
	Clean, Repaired, Reread, ECCRecovered int
	Descrambled                           int // read through from the audio before the track
	Bad                                   []int
	RepairedAt                            []int
	Seconds                               float64
}

func progress(label string, done, total int, t0 time.Time) {
	el := time.Since(t0).Seconds()
	rate := 0.0
	if el > 0 {
		rate = float64(done) * sectorRaw / el / 1e6
	}
	fmt.Printf("\r  %s  %5.1f%%  %.2f MB/s   ", label, 100*float64(done)/float64(total), rate)
}

// dumpData reads a Mode 1 data track, checking every sector against its own
// regeneration. Sectors with intact user data but wrong framing/parity bytes
// are corrected; sectors that fail are read again and, failing that, rebuilt
// from the drive's own error-corrected read. lead is written ahead of the
// data: the frames of the file that are recorded as audio (t.Lead of them).
func dumpData(dc *nativeDC, t nativeTrack, lead []byte, path string) (*nativeStats, error) {
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	w := bufio.NewWriterSize(f, 1<<20)
	w.Write(lead)
	fileStart := t.Start
	end := t.Start + t.Sectors
	t.Start += t.Lead
	t.Sectors -= t.Lead
	st := &nativeStats{}
	t0 := time.Now()
	label := fmt.Sprintf("Track %02d", t.Num)
	sec := make([]byte, sectorRaw)
	var pending []int // frames to read again
	done := 0
	for done < t.Sectors {
		s, err := dc.stream("read", t.Start+done, t.Sectors-done)
		if err != nil {
			return nil, err
		}
		for done < t.Sectors {
			if _, err := io.ReadFull(s, sec); err != nil {
				break
			}
			fad := t.Start + done
			state, fixed := CheckMode1(sec, fad)
			switch state {
			case SectorClean:
				st.Clean++
			case SectorRepairable:
				st.Repaired++
				st.RepairedAt = append(st.RepairedAt, fad)
			default:
				pending = append(pending, fad)
			}
			w.Write(fixed)
			done++
			if done%4096 == 0 {
				progress(label, done, t.Sectors, t0)
			}
		}
		s.Close()
		if done < t.Sectors {
			// The stream stopped at an unreadable sector. Reads are issued in
			// pairs, so set a pair aside and carry on behind it.
			for k := 0; k < 2 && done < t.Sectors; k++ {
				pending = append(pending, t.Start+done)
				w.Write(make([]byte, sectorRaw))
				done++
			}
		}
	}
	progress(label, done, t.Sectors, t0)
	fmt.Println()
	if err := w.Flush(); err != nil {
		return nil, err
	}

	var through []byte
	if t.Lead >= 2 && len(pending) > 0 && pending[0] < t.Start+transitionSpan {
		// A read that starts in the audio ahead of the track carries on in
		// audio mode across the boundary and returns the first data sectors
		// scrambled, which a read starting on them cannot fetch at all.
		through, _ = dc.readRaw(t.Start-2, 2+transitionSpan)
	}
	for _, fad := range pending {
		var good []byte
		if fad < t.Start+transitionSpan {
			if good = descrambledSector(through, fad); good != nil {
				st.Descrambled++
			}
		}
		// reads are issued in pairs and must not run past the end of the area
		pair := fad
		if pair+2 > end {
			pair = fad - 1
		}
		for try := 0; try < nativeRetries && good == nil && pair >= t.Start; try++ {
			// move the head away first: an immediate re-read tends to return the same bytes
			dc.readRaw(t.Start+(fad-t.Start+t.Sectors/2)%t.Sectors&^1, 2)
			if buf, err := dc.readRaw(pair, 2); err == nil && len(buf) >= (fad-pair+1)*sectorRaw {
				if state, fixed := CheckMode1(buf[(fad-pair)*sectorRaw:][:sectorRaw], fad); state != SectorBad {
					good = fixed
					st.Reread++
				}
			}
		}
		if good == nil {
			if s, err := dc.stream("cooked", fad-150, 1); err == nil {
				user := make([]byte, sectorUser)
				if _, err := io.ReadFull(s, user); err == nil {
					good = Mode1Sector(user, fad)
					st.ECCRecovered++
				}
				s.Close()
			}
		}
		if good == nil {
			st.Bad = append(st.Bad, fad)
			continue
		}
		if _, err := f.WriteAt(good, int64(fad-fileStart)*sectorRaw); err != nil {
			return nil, err
		}
	}
	st.Seconds = time.Since(t0).Seconds()
	return st, nil
}

// descrambledSector looks in an audio-mode read for the still scrambled data
// sector fad and returns it regenerated, or nil unless its user data passes
// EDC. The end of the last such sector is cut off where the drive switches to
// data mode; that only costs parity bytes, which the regeneration restores.
func descrambledSector(stream []byte, fad int) []byte {
	for i := 0; i+sectorRaw-64 <= len(stream); i++ {
		if stream[i] != 0 || stream[i+1] != 0xFF || ScrambledHeaderFAD(stream[i:]) != fad {
			continue
		}
		sec := make([]byte, sectorRaw)
		copy(sec, stream[i:])
		for k := 12; k < sectorRaw; k++ {
			sec[k] ^= scrambleTable[k-12]
		}
		if state, fixed := CheckMode1(sec, fad); state != SectorBad {
			return fixed
		}
	}
	return nil
}

// readAudio returns the track's samples: the raw stream shifted by offset bytes.
func readAudio(dc *nativeDC, t nativeTrack, offset int, label string) ([]byte, error) {
	first := t.Start + offset/sectorRaw
	skip := offset % sectorRaw
	need := t.Sectors + 1
	need += need & 1
	s, err := dc.stream("read", first, need)
	if err != nil {
		return nil, err
	}
	defer s.Close()
	buf := make([]byte, need*sectorRaw)
	t0 := time.Now()
	n := 0
	for n < len(buf) {
		m, err := s.Read(buf[n:min(len(buf), n+1<<20)])
		n += m
		progress(label, n/sectorRaw, need, t0)
		if err != nil {
			break
		}
	}
	fmt.Println()
	want := skip + t.Sectors*sectorRaw
	if n < want {
		return nil, fmt.Errorf("audio read ended after %d of %d sectors", n/sectorRaw, need)
	}
	return buf[skip:want], nil
}

func trackFileName(fullName string, num, total int) string {
	if total >= 10 {
		return fmt.Sprintf("%s (Track %02d).bin", fullName, num)
	}
	return fmt.Sprintf("%s (Track %d).bin", fullName, num)
}

func runNative(o Options) {
	auto := !o.Manual
	host := o.Native
	if host == "" || host == "default" {
		host = os.Getenv(nativeHostEnv)
	}
	if host == "" {
		fatal("--native needs the console's address: --native=HOST, or set " + nativeHostEnv + ".")
	}
	if !strings.Contains(host, ":") {
		host = net.JoinHostPort(host, nativePort)
	}
	dc := &nativeDC{addr: host}

	fmt.Printf("Dreamcast at %s...\n", host)
	info, err := dc.text("info")
	if err != nil {
		fatal(fmt.Sprintf("Cannot reach the Dreamcast disc service (%v). Is the console booted into Linux and on the network?", err))
	}
	kernel, firmware := "?", "?"
	if l := strings.Split(info, "\n"); len(l) >= 3 {
		kernel = strings.TrimSpace(l[0])
		var page []byte
		for _, h := range strings.Fields(l[2]) {
			if v, err := strconv.ParseUint(h, 16, 8); err == nil {
				page = append(page, byte(v))
			}
		}
		if len(page) >= 26 {
			firmware = strings.Join(strings.Fields(string(page[10:26])), " ")
		}
	}
	// Whatever answers on this port has to be the disc service on a console:
	// it reports the GD-ROM drive's own identification and raw 2352-byte reads.
	if firmware == "?" || firmware == "" || !reRawSize.MatchString(info) {
		fatal("The host answered, but not like the Dreamcast disc service (no GD-ROM drive identification or raw sector size):\n" + info)
	}
	fmt.Printf("  %s, GD-ROM drive: %s\n", kernel, firmware)

	var toc *nativeTOC
	var tocText string
	for try := 0; try < 3; try++ {
		// right after a disc change the first TOC request can fail on the pending attention condition
		if tocText, err = dc.text("toc"); err != nil {
			fatal("TOC request failed: " + err.Error())
		}
		toc = parseNativeTOC(tocText)
		if toc.LDLeadout != 0 && toc.HDLeadout != 0 {
			break
		}
		time.Sleep(time.Second)
	}
	if toc.LDLeadout == 0 || toc.HDLeadout == 0 || len(toc.Tracks) < 3 {
		fatal("The disc in the Dreamcast is not a GD-ROM (no table of contents for both areas):\n" + tocText)
	}
	var desc []string
	for _, t := range toc.Tracks {
		kind := "data"
		if t.Audio {
			kind = "audio"
		}
		desc = append(desc, fmt.Sprintf("%d %s@%d", t.Num, kind, t.FAD-150))
	}
	fmt.Printf("  TOC: %s; lead-outs@%d, %d\n", strings.Join(desc, ", "), toc.LDLeadout-150, toc.HDLeadout-150)
	if toc.Tracks[0].Audio || toc.Tracks[0].FAD != ldStartFAD {
		fatal("Track 1 is not a data track at LBA 0.")
	}

	var ip *IPBin
	if first, err := dc.readRaw(ldStartFAD, 2); err == nil && len(first) >= sectorRaw {
		if ip, err = ParseIPBin(first[16 : 16+sectorUser]); err != nil {
			fmt.Printf("  [!] LBA 0: %v\n", err)
		} else {
			fmt.Printf("  IP.BIN: %s  %s  V%s  %s  area %s  disc %d/%d\n", ip.Product, ip.Title, ip.Version, ip.Date, ip.Areas, ip.Disc, ip.Of)
		}
	} else {
		fatal("Cannot read LBA 0 from the Dreamcast.")
	}
	fmt.Println()

	db := LoadDAT()
	disc := IdentifyDisc(auto, ip, db, o.SerialArg)
	fullName := disc.FullName()
	entry := disc.DatEntry

	tracks, how, err := nativeLayout(toc, entry)
	if err != nil && entry != nil {
		fmt.Printf("  [!] %v — falling back to pregap rules.\n", err)
		tracks, how, err = nativeLayout(toc, nil)
	}
	if err != nil {
		fatal(err.Error())
	}
	fmt.Printf("  Layout: %s\n", how)

	offset, haveOffset := 0, false
	for i, t := range tracks {
		if t.Audio && i > 0 && !tracks[i-1].Audio && tracks[i-1].HD == t.HD {
			if offset, err = audioOffset(dc, t.Start); err != nil {
				fmt.Printf("  [!] Audio offset: %v\n", err)
			} else {
				haveOffset = true
				fmt.Printf("  Audio offset: %+d samples (%d bytes), measured after track %d\n", offset/4, offset, tracks[i-1].Num)
			}
			break
		}
	}
	fmt.Println()

	dir := resolveOutputDir(auto, fullName+" [native]")
	report := &strings.Builder{}
	fmt.Fprintf(report, "dcdump %s --native verification report\n%s\n\n", version, time.Now().Format("2006-01-02 15:04:05 -0700"))
	fmt.Fprintf(report, "Source: Dreamcast at %s, %s, GD-ROM drive %s\n", host, kernel, firmware)
	fmt.Fprintf(report, "Service info:\n%s\n", strings.TrimRight(info, "\n"))
	fmt.Fprintf(report, "This is an independent read for comparing hashes. It is not a redump submission:\n")
	fmt.Fprintf(report, "the drive has no subchannel or C2 output and is not an accepted dumping drive.\n\n")
	fmt.Fprintf(report, "Disc: %s\nTOC:\n%s\nLayout: %s\n", fullName, strings.TrimRight(tocText, "\n"), how)
	if haveOffset {
		fmt.Fprintf(report, "Audio offset: %+d samples (%d bytes)\n", offset/4, offset)
	}
	fmt.Fprintln(report)

	fmt.Println("Reading tracks...")
	var cue []CueTrack
	var fails []string
	for _, t := range tracks {
		name := trackFileName(fullName, t.Num, len(tracks))
		path := filepath.Join(dir, name)
		ct := CueTrack{Num: t.Num, File: name, Audio: t.Audio, HD: t.HD, Sectors: int64(t.Sectors)}
		fmt.Fprintf(report, "Track %02d: frames %d-%d (%d sectors), ", t.Num, t.Start, t.Start+t.Sectors-1, t.Sectors)
		if t.Audio {
			if !haveOffset {
				fails = append(fails, fmt.Sprintf("track %d: audio offset unknown, not read", t.Num))
				fmt.Fprintf(report, "audio, NOT READ (no offset)\n")
				continue
			}
			a, err := readAudio(dc, t, offset, fmt.Sprintf("Track %02d", t.Num))
			if err != nil {
				fatal(fmt.Sprintf("Track %d: %v", t.Num, err))
			}
			// audio has no checksum of its own: read it twice and compare
			b, err := readAudio(dc, t, offset, fmt.Sprintf("Track %02d again", t.Num))
			if err != nil {
				fatal(fmt.Sprintf("Track %d: %v", t.Num, err))
			}
			same := bytes.Equal(a, b)
			if !same {
				fails = append(fails, fmt.Sprintf("track %d: two audio reads differ", t.Num))
			}
			if err := os.WriteFile(path, a, 0o644); err != nil {
				fatal(err.Error())
			}
			fmt.Fprintf(report, "audio, two reads identical: %v\n", same)
		} else {
			var lead []byte
			if t.Lead > 0 {
				if !haveOffset {
					fails = append(fails, fmt.Sprintf("track %d: audio offset unknown, not read", t.Num))
					fmt.Fprintf(report, "data after audio, NOT READ (no offset)\n")
					continue
				}
				// the head of the file is the gap left by the audio track before it
				gap := nativeTrack{nativeTOCTrack: t.nativeTOCTrack, Start: t.Start, Sectors: t.Lead}
				var again []byte
				if lead, err = readAudio(dc, gap, offset, fmt.Sprintf("Track %02d gap", t.Num)); err == nil {
					again, err = readAudio(dc, gap, offset, fmt.Sprintf("Track %02d gap again", t.Num))
				}
				if err != nil {
					fatal(fmt.Sprintf("Track %d: %v", t.Num, err))
				}
				same := bytes.Equal(lead, again)
				if !same {
					fails = append(fails, fmt.Sprintf("track %d: two reads of the leading audio gap differ", t.Num))
				}
				fmt.Fprintf(report, "%d frames of audio gap (two reads identical: %v), then ", t.Lead, same)
			}
			st, err := dumpData(dc, t, lead, path)
			if err != nil {
				fatal(fmt.Sprintf("Track %d: %v", t.Num, err))
			}
			fmt.Printf("    %d clean, %d framing/parity repaired, %d good on re-read, %d descrambled from an audio-mode read, %d rebuilt from the drive's corrected read, %d bad (%.0f s)\n",
				st.Clean, st.Repaired, st.Reread, st.Descrambled, st.ECCRecovered, len(st.Bad), st.Seconds)
			fmt.Fprintf(report, "data: %d clean, %d repaired (framing/parity only), %d good on re-read, %d descrambled from an audio-mode read, %d rebuilt from corrected read, %d bad\n",
				st.Clean, st.Repaired, st.Reread, st.Descrambled, st.ECCRecovered, len(st.Bad))
			if len(st.RepairedAt) > 0 {
				fmt.Fprintf(report, "  repaired frames: %s\n", intList(st.RepairedAt, 40))
			}
			if len(st.Bad) > 0 {
				fmt.Fprintf(report, "  BAD frames: %s\n", intList(st.Bad, 40))
				fails = append(fails, fmt.Sprintf("track %d: %d unreadable sectors", t.Num, len(st.Bad)))
			}
		}
		sum, _, err := sha1File(path)
		if err != nil {
			fatal(err.Error())
		}
		ct.SHA1 = sum
		cue = append(cue, ct)
	}
	fmt.Println()

	if entry == nil || len(disc.Candidates) > 1 || disc.IdentifyByHash {
		alts := disc.Candidates
		if disc.IdentifyByHash || len(alts) == 0 {
			alts = db.All
		}
		if m := MatchByHash(cue, alts); m != nil {
			entry = m
			fmt.Printf("  DAT entry by hash: %s\n\n", m.Name)
		}
	}
	fails = append(fails, VerifyTracks(cue, entry, false)...)

	fmt.Fprintln(report)
	for _, t := range cue {
		fmt.Fprintf(report, "Track %02d SHA-1 %s  %s\n", t.Num, t.SHA1, t.File)
	}
	if entry != nil {
		fmt.Fprintf(report, "\nCompared with redump DAT entry: %s\n", entry.Name)
	}
	if len(fails) == 0 {
		fmt.Fprintf(report, "Result: every track matches redump.\n")
	} else {
		fmt.Fprintf(report, "Result: MISMATCH\n")
		for _, f := range fails {
			fmt.Fprintf(report, "  - %s\n", f)
		}
	}
	reportPath := filepath.Join(dir, fullName+".native.txt")
	os.WriteFile(reportPath, []byte(report.String()), 0o644)

	if len(fails) > 0 {
		lines := []string{"[!!!] NATIVE READ DOES NOT MATCH REDUMP:"}
		for _, f := range fails {
			lines = append(lines, "  - "+f)
		}
		lines = append(lines, "Output kept for analysis: "+keepFailed(dir))
		box(lines)
		os.Exit(1)
	}
	if o.Keep {
		os.WriteFile(filepath.Join(dir, fullName+".gdi"), []byte(GDIText(cue)), 0o644)
	} else {
		for _, t := range cue {
			os.Remove(filepath.Join(dir, t.File))
		}
	}
	notes := []string{
		"Note: read natively on a Dreamcast as an independent hash source; not a redump submission.",
		"Report: " + reportPath,
	}
	if !o.Keep {
		notes = append(notes, "Track files removed after verification (use --keep to keep them and a .gdi).")
	}
	PrintSummary(dir, cue, notes)
	fmt.Println("All checks passed: the Dreamcast's own drive reads every track as redump has it.")
}

func intList(v []int, max int) string {
	var s []string
	for i, x := range v {
		if i == max {
			s = append(s, fmt.Sprintf("... (%d more)", len(v)-max))
			break
		}
		s = append(s, strconv.Itoa(x))
	}
	return strings.Join(s, " ")
}

// sha1Bytes is used by the tests.
func sha1Bytes(b []byte) string {
	h := sha1.Sum(b)
	return hex.EncodeToString(h[:])
}
