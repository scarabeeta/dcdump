package main

import (
	"fmt"
	"time"
)

// Drive spin-up state checks, selected per drive-list entry with "state_check".
//
// "ts-h492c": TS-H492C GD firmware GD24 or later. The firmware records the outcome of every
// spin-up attempt in XRAM 0x8B78 and keeps the measured spin-up level in 0x8B86. Both are read
// through the firmware's read-only REQUEST SENSE window (CDB 03 AH 20 AL 12 00 returns
// XRAM[AH:AL..+3] in the sense information bytes); there is no write path. A disc that comes up
// without the low-level servo path (status bit0 clear) cannot seek reliably in the HD area.

const (
	h492cStatusAddr = 0x8B78
	h492cLevelAddr  = 0x8B86
)

// H492CState is the decoded TS-H492C spin-up status byte.
type H492CState struct {
	Raw      byte
	Level    byte
	Valid    bool // bit7: written at the end of the last spin-up attempt
	LowLevel bool // bit0: IRAM 0x2B.5 set, servo parameter sets learned
	Promoted bool // bit1: the GD1Z promotion ran for this medium
	PromFail bool // bit2: promoted, but that attempt failed
	Failed   bool // bit3: the last spin-up attempt failed
}

func DecodeH492CState(status, level byte) H492CState {
	return H492CState{
		Raw: status, Level: level,
		Valid:    status&0x80 != 0,
		LowLevel: status&0x01 != 0,
		Promoted: status&0x02 != 0,
		PromFail: status&0x04 != 0,
		Failed:   status&0x08 != 0,
	}
}

// Good reports whether HD seeks can be trusted on this spin-up.
func (s H492CState) Good() bool { return s.Valid && s.LowLevel && !s.Failed && !s.PromFail }

func (s H492CState) String() string {
	path := "stock pressed-CD path"
	switch {
	case s.LowLevel && s.Promoted:
		path = "low-level path (promoted)"
	case s.LowLevel:
		path = "low-level path"
	}
	str := fmt.Sprintf("status 0x%02X, spin-up level 0x%02X, %s", s.Raw, s.Level, path)
	if s.PromFail {
		str += ", promoted attempt failed"
	}
	if s.Failed {
		str += ", spin-up failed"
	}
	if !s.Valid {
		str += ", not valid"
	}
	return str
}

// peekXRAM reads 4 bytes of drive XRAM through the TS-H492C read-only window.
func (d *SCSIDevice) peekXRAM(addr uint16) ([]byte, Sense) {
	buf, s := d.exec([]byte{0x03, byte(addr >> 8), 0x20, byte(addr), 0x12, 0}, 18, nil, 10*time.Second)
	if !s.OK || len(buf) < 7 {
		return nil, s
	}
	return buf[3:7], s
}

// CheckDriveState runs the entry's state check, if any. It returns a line to print and, when the
// drive is not in a state to dump the HD area, an error message for the user.
func CheckDriveState(dev *SCSIDevice, p *Profile) (string, string) {
	switch p.StateCheck {
	case "":
		return "", ""
	case "ts-h492c":
		st, s1 := dev.peekXRAM(h492cStatusAddr)
		lv, s2 := dev.peekXRAM(h492cLevelAddr)
		if st == nil || lv == nil {
			return "", fmt.Sprintf("Cannot read the drive state (%s / %s); is the firmware GD24 or later?", s1, s2)
		}
		state := DecodeH492CState(st[0], lv[0])
		line := "  Drive state: " + state.String()
		if !state.Good() {
			return line, "The drive came up in the bad state (HD seeks would fail or be very slow): eject and reload the disc, then run dcdump again."
		}
		return line + " — good", ""
	}
	return "", fmt.Sprintf("unknown state_check %q", p.StateCheck)
}
