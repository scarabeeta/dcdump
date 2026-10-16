package main

// Direct SCSI commands through the Linux SG_IO ioctl. Transfer lengths are always
// exact: an over-length allocation sends some USB bridges (ASMedia ASM1153) into a
// broken PIO path.

import (
	"encoding/binary"
	"fmt"
	"os"
	"runtime"
	"syscall"
	"time"
	"unsafe"
)

const (
	sgIO          = 0x2285
	sgDxferNone   = -1
	sgDxferToDev  = -2
	sgDxferFromDv = -3
)

// sg_io_hdr from <scsi/sg.h> (64-bit layout, 88 bytes).
type sgIoHdr struct {
	InterfaceID    int32
	DxferDirection int32
	CmdLen         uint8
	MxSbLen        uint8
	IovecCount     uint16
	DxferLen       uint32
	Dxferp         uintptr
	Cmdp           uintptr
	Sbp            uintptr
	Timeout        uint32
	Flags          uint32
	PackID         int32
	UsrPtr         uintptr
	Status         uint8
	MaskedStatus   uint8
	MsgStatus      uint8
	SbLenWr        uint8
	HostStatus     uint16
	DriverStatus   uint16
	Resid          int32
	Duration       uint32
	Info           uint32
}

type SCSIDevice struct {
	f *os.File
}

// Sense is the outcome of one command: OK, or the fixed-format sense key/ASC/ASCQ.
type Sense struct {
	OK             bool
	Key, ASC, ASCQ byte
	Status         byte
	Host, Driver   uint16
	Resid          int32
	// GoodWithSense: the kernel reported SCSI status GOOD although the drive returned sense
	// data with an error sense key (libata regression 2e1d2e65e773, fixed by bc8ce2cea5f7).
	GoodWithSense bool
}

func (s Sense) String() string {
	if s.OK {
		return "OK"
	}
	if s.Status == 2 || s.Key != 0 {
		return fmt.Sprintf("%X/%02X/%02X", s.Key, s.ASC, s.ASCQ)
	}
	return fmt.Sprintf("status %d host %d driver %d resid %d", s.Status, s.Host, s.Driver, s.Resid)
}

func OpenSCSI(path string) (*SCSIDevice, error) {
	f, err := os.OpenFile(path, os.O_RDWR|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	return &SCSIDevice{f: f}, nil
}

func (d *SCSIDevice) Close() { d.f.Close() }

// exec runs one CDB. din > 0 reads din bytes; dout != nil writes it.
func (d *SCSIDevice) exec(cdb []byte, din int, dout []byte, timeout time.Duration) ([]byte, Sense) {
	sense := make([]byte, 32)
	h := sgIoHdr{
		InterfaceID: 'S',
		CmdLen:      uint8(len(cdb)),
		MxSbLen:     uint8(len(sense)),
		Cmdp:        uintptr(unsafe.Pointer(&cdb[0])),
		Sbp:         uintptr(unsafe.Pointer(&sense[0])),
		Timeout:     uint32(timeout / time.Millisecond),
	}
	var buf []byte
	switch {
	case dout != nil:
		buf = dout
		h.DxferDirection = sgDxferToDev
	case din > 0:
		buf = make([]byte, din)
		h.DxferDirection = sgDxferFromDv
	default:
		h.DxferDirection = sgDxferNone
	}
	if len(buf) > 0 {
		h.DxferLen = uint32(len(buf))
		h.Dxferp = uintptr(unsafe.Pointer(&buf[0]))
	}
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, d.f.Fd(), sgIO, uintptr(unsafe.Pointer(&h)))
	runtime.KeepAlive(cdb)
	runtime.KeepAlive(sense)
	runtime.KeepAlive(buf)
	s := Sense{Status: h.Status, Host: h.HostStatus, Driver: h.DriverStatus, Resid: h.Resid}
	if errno != 0 {
		s.Host = 0xFFFF
		return nil, s
	}
	s.OK = h.Status == 0 && h.HostStatus == 0 && h.DriverStatus&0xF == 0 && h.Resid == 0
	// Sense data with an error key means the command failed, whatever the status byte says:
	// some kernels report rejected ATAPI commands (ILLEGAL REQUEST) with GOOD status.
	if h.SbLenWr > 0 && sense[2]&0xF > 1 {
		s.GoodWithSense = h.Status == 0
		s.OK = false
	}
	if !s.OK && h.SbLenWr > 0 {
		s.Key, s.ASC, s.ASCQ = sense[2]&0xF, sense[12], sense[13]
	}
	if din > 0 {
		return buf, s
	}
	return nil, s
}

// KernelHidesCheckCondition sends a READ TOC the drive rejects on CD/GD media (format 4, ATIP
// of a pressed disc) and reports whether the kernel returned that ILLEGAL REQUEST with GOOD
// status. With such a kernel, tools that check only the status byte (redumper) take rejected
// commands as successful and keep the stale buffer.
func (d *SCSIDevice) KernelHidesCheckCondition() (bool, Sense) {
	_, s := d.exec([]byte{0x43, 0x02, 0x04, 0, 0, 0, 0, 0x10, 0x00, 0}, 0x1000, nil, 10*time.Second)
	return s.GoodWithSense, s
}

func (d *SCSIDevice) TestUnitReady() Sense {
	_, s := d.exec([]byte{0x00, 0, 0, 0, 0, 0}, 0, nil, 10*time.Second)
	return s
}

// WaitReady polls TEST UNIT READY. Returns the last sense on timeout.
func (d *SCSIDevice) WaitReady(limit time.Duration) Sense {
	deadline := time.Now().Add(limit)
	var s Sense
	for {
		s = d.TestUnitReady()
		if s.OK || time.Now().After(deadline) {
			return s
		}
		if s.Key == 2 && s.ASC == 0x3A { // medium not present: no point waiting
			return s
		}
		time.Sleep(time.Second)
	}
}

// AllowMediumRemoval clears a tray lock left behind when sr was unbound while open.
func (d *SCSIDevice) AllowMediumRemoval() Sense {
	_, s := d.exec([]byte{0x1E, 0, 0, 0, 0, 0}, 0, nil, 10*time.Second)
	return s
}

type TOCTrack struct {
	Number  int // 0xAA = lead-out
	Control byte
	LBA     int
}

func (t TOCTrack) IsData() bool { return t.Control&4 != 0 }

// ReadTOC returns the format-0 TOC (LBA addressing), lead-out included.
func (d *SCSIDevice) ReadTOC() ([]TOCTrack, Sense) {
	hdr, s := d.exec([]byte{0x43, 0, 0, 0, 0, 0, 0, 0, 4, 0}, 4, nil, 30*time.Second)
	if !s.OK {
		return nil, s
	}
	n := int(binary.BigEndian.Uint16(hdr[0:2])) + 2
	data, s := d.exec([]byte{0x43, 0, 0, 0, 0, 0, 0, byte(n >> 8), byte(n), 0}, n, nil, 30*time.Second)
	if !s.OK {
		return nil, s
	}
	var tracks []TOCTrack
	for off := 4; off+8 <= len(data); off += 8 {
		e := data[off : off+8]
		tracks = append(tracks, TOCTrack{Number: int(e[2]), Control: e[1] & 0xF, LBA: int(int32(binary.BigEndian.Uint32(e[4:8])))})
	}
	return tracks, s
}

// Read10 reads cooked 2048-byte sectors.
func (d *SCSIDevice) Read10(lba, count int) ([]byte, Sense) {
	cdb := []byte{0x28, 0, byte(lba >> 24), byte(lba >> 16), byte(lba >> 8), byte(lba), 0, byte(count >> 8), byte(count), 0}
	return d.exec(cdb, 2048*count, nil, 30*time.Second)
}

// ReadCDRaw reads count raw 2352-byte sectors (all header/EDC fields, no subchannel).
func (d *SCSIDevice) ReadCDRaw(lba, count int) ([]byte, Sense) {
	cdb := []byte{0xBE, 0, byte(lba >> 24), byte(lba >> 16), byte(lba >> 8), byte(lba), byte(count >> 16), byte(count >> 8), byte(count), 0xF8, 0, 0}
	return d.exec(cdb, 2352*count, nil, 60*time.Second)
}
