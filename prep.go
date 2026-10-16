package main

// Drive preparation before a dump:
//   USB:  force the bridge onto USB 2.0 (SuperSpeed links reset mid-read),
//   both: unbind sr so desktop automount probes don't interleave commands,
//         and send ALLOW MEDIUM REMOVAL (unbinding sr while it was open leaves
//         the tray locked).
// sysfs writes go through `sudo -n tee`; without passwordless sudo they are
// reported and skipped.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

func sudoWrite(path, value string) error {
	cmd := exec.Command("sudo", "-n", "tee", path)
	cmd.Stdin = strings.NewReader(value)
	cmd.Stdout = nil
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// innermost USB device in a sysfs path, e.g. "4-6" or "4-6.2"
var reUSBDev = regexp.MustCompile(`/(\d+-[0-9.]+)/`)

func usbDevOf(host string) string {
	real, err := filepath.EvalSymlinks("/sys/class/scsi_device/" + host + ":0:0:0/device")
	if err != nil {
		return ""
	}
	m := reUSBDev.FindAllStringSubmatch(real, -1)
	if m == nil {
		return ""
	}
	return m[len(m)-1][1]
}

// PrepareDrive returns the (possibly re-enumerated) drive.
func PrepareDrive(d *Drive, profiles []*Profile) *Drive {
	fmt.Println("Preparing drive...")
	if d.Transport == "USB" {
		d = forceUSB2(d, profiles)
	}
	if d.Transport == "SATA" {
		checkATAMode(d)
	}
	hctl := d.Host + ":0:0:0"
	if _, err := os.Stat("/sys/bus/scsi/drivers/sr/" + hctl); err == nil {
		if err := sudoWrite("/sys/bus/scsi/drivers/sr/unbind", hctl); err != nil {
			fmt.Printf("  [!] Could not unbind sr from %s (%v); desktop automount may interfere.\n", hctl, err)
		} else {
			fmt.Printf("  sr unbound from %s\n", hctl)
			d.SR = ""
		}
	}
	dev, err := OpenSCSI(d.SG)
	if err != nil {
		fatal(fmt.Sprintf("Cannot open %s: %v", d.SG, err))
	}
	defer dev.Close()
	dev.WaitReady(10 * time.Second)
	if s := dev.AllowMediumRemoval(); s.OK {
		fmt.Println("  Tray unlocked (ALLOW MEDIUM REMOVAL)")
	}
	fmt.Println()
	return d
}

// forceUSB2 disables the drive's USB port for 3 s through its sysfs `port` link (also behind
// hubs) so the bridge re-enumerates at high speed. The drive is found again as the DH16D2S-
// type USB drive whose USB device is not one of the other drives' (they stay where they are);
// matching by model/revision alone would confuse two identical drives.
func forceUSB2(d *Drive, profiles []*Profile) *Drive {
	devName := usbDevOf(d.Host)
	if devName == "" {
		return d
	}
	speed, _ := os.ReadFile("/sys/bus/usb/devices/" + devName + "/speed")
	sp := strings.TrimSpace(string(speed))
	if sp == "480" || sp == "" {
		fmt.Printf("  USB link: %s Mbit/s\n", sp)
		return d
	}
	port, err := filepath.EvalSymlinks("/sys/bus/usb/devices/" + devName + "/port")
	if err != nil {
		fmt.Printf("  [!] USB link is %s Mbit/s and %s has no port control; SuperSpeed may reset mid-read.\n", sp, devName)
		return d
	}
	others := map[string]bool{}
	for _, o := range GetCDDrives(profiles) {
		if o.Transport == "USB" && o.SG != d.SG {
			others[usbDevOf(o.Host)] = true
		}
	}
	fmt.Printf("  USB link is %s Mbit/s; re-enumerating at USB 2.0...\n", sp)
	if err := sudoWrite(port+"/disable", "1"); err != nil {
		fmt.Printf("  [!] Could not disable %s (%v); continuing at %s Mbit/s.\n", port, err, sp)
		return d
	}
	time.Sleep(3 * time.Second)
	if err := sudoWrite(port+"/disable", "0"); err != nil {
		fatal(fmt.Sprintf("Could not re-enable %s: %v", port, err))
	}
	for i := 0; i < 30; i++ {
		time.Sleep(time.Second)
		for _, nd := range GetCDDrives(profiles) {
			if nd.Transport == "USB" && nd.Vendor == d.Vendor && nd.Model == d.Model && !others[usbDevOf(nd.Host)] {
				if d.Profile != nil {
					nd.Profile = d.Profile // keep a --force-drive assignment
				}
				time.Sleep(2 * time.Second)
				fmt.Printf("  Re-enumerated: %s\n", nd.SG)
				return nd
			}
		}
	}
	fatal("The drive did not come back after switching to USB 2.0.")
	return nil
}

var reATAPort = regexp.MustCompile(`/ata(\d+)/`)

// checkATAMode warns when libata has stepped the link down to PIO (after ATA bus errors it can
// fall back as far as PIO0 and stays there until reboot). C2 reads whose length is not a multiple
// of 16 already go over PIO; in PIO mode multi-sector C2 reads fail on the D2S.
func checkATAMode(d *Drive) {
	real, err := filepath.EvalSymlinks("/sys/class/scsi_device/" + d.Host + ":0:0:0/device")
	if err != nil {
		return
	}
	m := reATAPort.FindStringSubmatch(real)
	if m == nil {
		return
	}
	devs, _ := filepath.Glob("/sys/class/ata_device/dev" + m[1] + ".*")
	if len(devs) != 1 {
		return
	}
	b, err := os.ReadFile(devs[0] + "/xfer_mode")
	if err != nil {
		return
	}
	mode := strings.TrimSpace(string(b))
	if strings.Contains(mode, "PIO") {
		box([]string{
			fmt.Sprintf("[!] ata%s runs at %s: libata stepped the link down after bus errors.", m[1], mode),
			"The dump may still work, but it is slower and multi-sector C2 reads fail in PIO.",
			"Reboot the host to restore DMA before important dumps.",
		})
		return
	}
	fmt.Printf("  ata%s transfer mode: %s\n", m[1], mode)
}
