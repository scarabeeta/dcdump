package main

// redumper invocation for the LD area, dumped as a plain CD (no --rings: the HD area is
// dumped by dreamdump, see dreamdump.go). The drive-list entry gives the speed and, for
// drives redumper does not know yet, the generic drive overrides and the sector order.
// redumper b765 and later carry DH16D2S GD0D, so those settings are left to redumper
// itself. For GD drives on USB, redumper-exact (upstream + PR superg/redumper#459, exact
// READ CD transfer length) is used because the ASM1153 bridge corrupts non-multiple-of-4
// transfers, and the sector order is still forced there because C2 cannot be transferred.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
)

type RunPlan struct {
	Binary string
	Exact  bool
	Known  bool // the drive is in this redumper build's own database
	Args   []string
}

const exactDisclaimer = "redumper upstream + PR superg/redumper#459 only: READ CD requests the exact transfer length; CDB, sector data, offsets and hashing are unchanged."

// SplitArgs separates dcdump's own options from the passthrough to redumper
// (space-separated values, --retries=N).
type Options struct {
	Manual     bool
	DriveArg   string
	SerialArg  string
	ForceDrive string
	Retries    string
	LDOnly     bool
	Check      bool
	Native     string // --native[=HOST]: read the disc in a Dreamcast over the network
	Keep       bool   // --keep: keep the track files of a --native read
	Help       bool   // -h/--help: print usage and exit without touching a drive
	Passthru   []string
}

// Usage is the --help text. An unknown option is rejected rather than forwarded, so that a
// typo cannot start a dump; redumper options go after a literal "--".
const Usage = `dcdump — Dreamcast GD-ROM dump tool

Usage:
  dcdump [options]                 auto mode (default): pick the GD drive, identify the
                                   disc, dump the LD and HD areas, verify against redump
  dcdump --manual [options]        prompt for drive / serial instead of auto-selecting
  dcdump --help                    show this help and exit (touches no drive)

Options:
  -h, --help          show this help and exit
  --manual            interactive drive and serial selection
  --ld-only           dump the single-density (LD) area only; skip the HD area
  --check             run drive and disc checks, then stop without dumping
  --drive VALUE       choose the drive by index or vendor/model substring
  --serial VALUE      force the disc serial (e.g. --serial T-20106M)
  --force-drive ID    use a drive-list entry's parameters for an unlisted drive
  --retries=N         redumper read retries (default 20)
  --native[=HOST]     read the disc in a networked Dreamcast instead of a PC drive
  --keep              keep the per-track files of a --native read
  -- ARGS...          pass ARGS to redumper unchanged (e.g. dcdump -- --speed=4)

Any other option is rejected, so a typo cannot start a dump by accident.`

func ParseOptions(argv []string) Options {
	o := Options{Retries: "20"}
	valueOf := map[string]*string{"--drive": &o.DriveArg, "--serial": &o.SerialArg, "--force-drive": &o.ForceDrive}
	need := map[string]string{
		"--drive":       "--drive requires a value (index or vendor/model substring).",
		"--serial":      "--serial requires a value, e.g. --serial T-20106M",
		"--force-drive": "--force-drive requires a drive-list entry id, e.g. --force-drive dh16d2s-gd",
	}
	// Everything after a literal "--" is forwarded to redumper verbatim. Before it, an
	// unknown option is a typo and must not silently start a dump.
	passthru := false
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		if passthru {
			o.Passthru = append(o.Passthru, a)
			continue
		}
		switch {
		case a == "--":
			passthru = true
		case a == "-h" || a == "--help":
			o.Help = true
		case a == "--manual":
			o.Manual = true
		case a == "--ld-only":
			o.LDOnly = true
		case a == "--check":
			o.Check = true
		case a == "--native":
			o.Native = "default"
		case strings.HasPrefix(a, "--native="):
			o.Native = strings.SplitN(a, "=", 2)[1]
		case a == "--keep":
			o.Keep = true
		case strings.HasPrefix(a, "--retries="):
			o.Retries = strings.SplitN(a, "=", 2)[1]
		case valueOf[a] != nil:
			if i+1 >= len(argv) {
				fatal(need[a])
			}
			*valueOf[a] = argv[i+1]
			i++
		case strings.HasPrefix(a, "-"):
			fatal("Unknown option " + a + ".\n" +
				"  Run 'dcdump --help' for the list of options.\n" +
				"  To pass an option to redumper, put it after --, e.g. dcdump -- --speed=4")
		default:
			fatal("Unexpected argument " + a + ".\n" +
				"  dcdump takes options only; run 'dcdump --help'.\n" +
				"  To pass arguments to redumper, put them after --.")
		}
	}
	return o
}

// BuildRun assembles the redumper command line for the LD area.
func BuildRun(d *Drive, o Options, sgPath, workDir, fullName string) RunPlan {
	p := d.Profile
	rp := RunPlan{Binary: "redumper"}
	if p != nil && p.GD && d.Transport != "SATA" {
		rp.Binary, rp.Exact = "redumper-exact", true
	}
	args := []string{"disc",
		"--drive=" + sgPath,
		"--image-path=" + workDir,
		"--image-name=" + fullName,
		"--retries=" + o.Retries,
	}
	if p != nil {
		if p.Speed > 0 {
			args = append(args, fmt.Sprintf("--speed=%d", p.Speed))
		}
		if p.ReadOffset != nil {
			order := p.SectorOrder["usb"]
			if d.Transport == "SATA" && p.SectorOrder["sata"] != "" {
				order = p.SectorOrder["sata"]
			}
			// A drive redumper already knows is configured from its own database; passing the
			// same values again would only mask a future correction there. The sector order is
			// still forced off SATA, where the bridge cannot carry C2.
			rp.Known = knownToRedumper(rp.Binary, d)
			if !rp.Known {
				args = append(args, "--drive-type=GENERIC", "--drive-read-method=BE",
					fmt.Sprintf("--drive-read-offset=%d", *p.ReadOffset))
			}
			if order != "" && (!rp.Known || d.Transport != "SATA") {
				args = append(args, "--drive-sector-order="+order)
			}
		}
	}
	rp.Args = append(args, o.Passthru...)
	return rp
}

// reKnownDrive matches one line of "redumper --list-recommended-drives", which reads
// "VENDOR - PRODUCT (revision level: REV, vendor specific: ...)".
var reKnownDrive = regexp.MustCompile(`^(.*?) - (.*?) \(revision level: ([^,]*),`)

// normalizeID trims and collapses whitespace, as redumper does before matching a drive
// against its database.
func normalizeID(s string) string { return strings.Join(strings.Fields(s), " ") }

// redumperKnowsDrive reports whether this redumper build lists the drive among the drives
// it recognizes, so that dcdump does not have to describe it.
func redumperKnowsDrive(bin string, d *Drive) bool {
	path, err := findBinary(bin)
	if err != nil {
		return false
	}
	out, err := exec.Command(path, "--list-recommended-drives").Output()
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(out), "\n") {
		m := reKnownDrive.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		if normalizeID(m[1]) == normalizeID(d.Vendor) && normalizeID(m[2]) == normalizeID(d.Model) &&
			normalizeID(m[3]) == normalizeID(d.Rev) {
			return true
		}
	}
	return false
}

// knownToRedumper is a variable so that tests can answer without a redumper on PATH.
var knownToRedumper = redumperKnowsDrive

var reBuild = regexp.MustCompile(`build:\s*([^)\s]+)`)

func redumperBuild(bin string) string {
	out, _ := exec.Command(bin, "--version").CombinedOutput()
	if m := reBuild.FindStringSubmatch(string(out)); m != nil {
		return m[1]
	}
	return "?"
}

// findBinary looks on PATH, then next to dcdump's own (unresolved) location and in
// ~/.local/bin (non-login shells, e.g. over ssh, often lack ~/.local/bin on PATH).
func findBinary(name string) (string, error) {
	p, err := exec.LookPath(name)
	if err == nil {
		return p, nil
	}
	home, _ := os.UserHomeDir()
	for _, dir := range []string{filepath.Dir(os.Args[0]), filepath.Join(home, ".local", "bin")} {
		c := filepath.Join(dir, name)
		if st, e := os.Stat(c); e == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
			return c, nil
		}
	}
	return "", err
}

// Progress is the error state parsed from redumper's progress line
// ("LBA:  8826/765000, errors: { SCSIs: 0, C2s: 0, Q: 1 }").
type Progress struct {
	LBA, SCSI, C2, Q int
}

var reProgress = regexp.MustCompile(`LBA:\s*(-?\d+)/\d+, errors: \{ SCSIs: (\d+), C2s: (\d+), Q: (\d+)`)

// EarlyAbortReason checks a progress sample during the initial dump pass. A run where the
// subchannel Q of nearly every sector is bad from the very start (seen once on GD0A, not
// reproducible: every LD sector failed Q, consistent with a one-sector subchannel lag)
// cannot produce a usable dump, so stop it instead of reading for another 10 minutes.
func EarlyAbortReason(p Progress) string {
	if p.LBA >= 1000 && 2*p.Q > p.LBA+1 {
		return fmt.Sprintf("subchannel Q bad on %d of the first %d sectors", p.Q, p.LBA+1)
	}
	return ""
}

// Run executes redumper, passing its output through while watching the progress line.
// Returns the build id and, if the run was stopped by the early check, the reason.
func (rp *RunPlan) Run() (string, string) {
	path, err := findBinary(rp.Binary)
	if err != nil {
		if rp.Exact {
			fatal("redumper-exact not found on PATH (needed for USB transport until PR superg/redumper#459 is released).")
		}
		fatal("redumper not found on PATH.")
	}
	build := redumperBuild(path)
	fmt.Println("Running redumper...")
	fmt.Printf("  %s (build: %s)\n", rp.Binary, build)
	if rp.Known {
		fmt.Println("  drive is in this redumper's database; its own settings are used")
	}
	if rp.Exact {
		fmt.Println("  [!] " + exactDisclaimer)
	}
	fmt.Printf("  %s %s\n", rp.Binary, strings.Join(rp.Args, " "))
	fmt.Println()
	cmd := exec.Command(path, rp.Args...)
	cmd.Stdin, cmd.Stderr = os.Stdin, os.Stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		fatal(err.Error())
	}
	if err := cmd.Start(); err != nil {
		fatal("redumper failed to start: " + err.Error())
	}
	abort := ""
	checking := true // only during the first dump pass (until the first section after DUMP)
	buf := make([]byte, 4096)
	var line []byte
	for {
		n, rerr := out.Read(buf)
		if n > 0 {
			os.Stdout.Write(buf[:n])
			for _, b := range buf[:n] {
				if b != '\r' && b != '\n' {
					line = append(line, b)
					continue
				}
				l := string(line)
				line = line[:0]
				if strings.HasPrefix(l, "*** DUMP::EXTRA") || strings.HasPrefix(l, "*** REFINE") {
					checking = false
				}
				if !checking || abort != "" {
					continue
				}
				if m := reProgress.FindStringSubmatch(l); m != nil {
					var p Progress
					fmt.Sscan(m[1], &p.LBA)
					fmt.Sscan(m[2], &p.SCSI)
					fmt.Sscan(m[3], &p.C2)
					fmt.Sscan(m[4], &p.Q)
					if r := EarlyAbortReason(p); r != "" {
						abort = r
						cmd.Process.Signal(syscall.SIGINT)
						proc := cmd.Process // SIGINT may be ignored (e.g. under nohup)
						time.AfterFunc(10*time.Second, func() { proc.Kill() })
					}
				}
			}
		}
		if rerr != nil {
			break
		}
	}
	werr := cmd.Wait()
	fmt.Println()
	if abort != "" {
		return build, abort
	}
	if werr != nil {
		fatal("redumper failed.")
	}
	return build, ""
}
