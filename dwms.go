// dwms is a dwm status generator.
//
// Assign custom values to exported identifiers in config.go to configure.
package main

import (
	"bytes"
	"container/ring"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/BurntSushi/xgb"
	"github.com/BurntSushi/xgb/xproto"
)

type statusFunc func() string

const (
	battSysPath = "/sys/class/power_supply"
	netSysPath  = "/sys/class/net"
)

var (
	ssidRE      = regexp.MustCompile(`SSID:\s+(.*)`)
	rxBytesRE   = regexp.MustCompile(`RX:\s+(\d+)`)
	txBytesRE   = regexp.MustCompile(`TX:\s+(\d+)`)
	signalRE    = regexp.MustCompile(`signal:\s+(-\d+)`)
	amixerRE    = regexp.MustCompile(`\[(\d+)%\].*\[(\w+)\]`)
	xconn       *xgb.Conn
	xroot       xproto.Window
	rxAvg       *ring.Ring
	txAvg       *ring.Ring
	lastRxBytes int
	lastTxBytes int
)

func formatBytes(bs int) string {
	const (
		kilo = 1_000
		mega = 1_000 * kilo
		giga = 1_000 * mega
	)
	val, unit := bs, "B"
	switch {
	case bs >= giga:
		val, unit = bs/giga, "G"
	case bs >= mega:
		val, unit = bs/mega, "M"
	case bs >= kilo:
		val, unit = bs/kilo, "K"
	}
	return strconv.Itoa(val) + unit
}

type format struct {
	emoji string
	text  string
	size  int
}

func (f format) String() string {
	size := len(f.emoji) + f.size + 1
	var sb strings.Builder
	sb.Grow(size)
	sb.WriteString(f.emoji)
	sb.WriteRune(' ')
	if len(f.text) > f.size {
		sb.WriteString(f.text[:f.size])
	} else {
		sb.WriteString(f.text)
		sb.WriteString(strings.Repeat(" ", f.size-len(f.text)))
	}
	return sb.String()
}

var WifiFmt = func(dev, ssid string, rxBytes, txBytes, signal int, up bool) string {
	if !up {
		return ""
	}
	rx := formatBytes(rxBytes)
	tx := formatBytes(rxBytes)
	ssig := strconv.Itoa(signal)
	fmts := []format{
		{"📡", ssid, 10},
		{"⬇", rx, 4},
		{"⬆", tx, 4},
		{"📶", ssig, 3},
	}
	var strs []string
	for _, f := range fmts {
		strs = append(strs, f.String())
	}
	return strings.Join(strs, " ")
}

var WiredFmt = func(dev string, speed int, up bool) string {
	if !up {
		return ""
	}
	return "[=" + strconv.Itoa(speed)
}

var NetFmt = func(devs []string) string {
	return strings.Join(filterEmpty(devs), " ")
}

var BatteryDevFmt = func(pct int, state string) string {
	spct := strconv.Itoa(pct)
	smoji := map[string]string{"Charging": "🔌", "Full": "🔌", "Discharging": "🔋"}[state]
	return format{smoji, spct, 3}.String()
}

var BatteryFmt = func(bats []string) string {
	return strings.Join(bats, "/")
}

var AudioFmt = func(vol int, muted bool) string {
	svol := strconv.Itoa(vol)
	volmoji := ""
	switch {
	case muted:
		volmoji = "🔇"
	case vol < 33:
		volmoji = "🔈"
	case vol < 66:
		volmoji = "🔉"
	default:
		volmoji = "🔊"
	}
	return format{volmoji, svol, 3}.String()
}

var TimeFmt = func(t time.Time) string {
	offsetTime := t.Add(time.Minute * 15)
	// get hour
	hour := offsetTime.Hour() % 12
	// get half-hour
	halfHour := (offsetTime.Minute() + 1) / 30
	clockEmojis := [24]string{
		"🕛", "🕧",
		"🕐", "🕜",
		"🕑", "🕝",
		"🕒", "🕞",
		"🕓", "🕟",
		"🕔", "🕠",
		"🕕", "🕡",
		"🕖", "🕢",
		"🕗", "🕣",
		"🕘", "🕤",
		"🕙", "🕥",
		"🕚", "🕦",
	}
	clockEmoji := clockEmojis[hour*2+halfHour]
	return t.Format("📅01/02/2006 " + clockEmoji + "15:04")
}

var StatusFmt = func(stats []string) string {
	return " " + strings.Join(filterEmpty(stats), " ") + " "
}

func getByteDiff(rxBytes, txBytes int) (int, int) {
	defer func() {
		lastRxBytes, lastTxBytes = rxBytes, txBytes
	}()
	if rxBytes < lastRxBytes {
		return 0, 0
	}
	if txBytes < lastTxBytes {
		return 0, 0
	}
	return rxBytes - lastRxBytes, txBytes - lastTxBytes
}

func getRollingAverage(roll *ring.Ring) int {
	sum := 0
	count := 0
	for i := 0; i < roll.Len(); i++ {
		val, ok := roll.Move(i).Value.(int)
		if ok {
			sum += val
			count++
		}
	}
	if count == 0 {
		return 0
	}
	return sum / count
}

func wifiStatus(dev string, up bool) (ssid string, rxBytes int, txBytes int, signal int) {
	if !up {
		rxAvg = ring.New(5)
		txAvg = ring.New(5)
		lastRxBytes = 0
		lastTxBytes = 0
		return
	}
	out, err := exec.Command("iw", "dev", dev, "link").Output()
	if err != nil {
		return
	}
	if match := ssidRE.FindSubmatch(out); len(match) >= 2 {
		ssid = string(match[1])
	}
	if match := rxBytesRE.FindSubmatch(out); len(match) >= 2 {
		if br, err := strconv.Atoi(string(match[1])); err == nil {
			rxBytes = br
		}
	}
	if match := txBytesRE.FindSubmatch(out); len(match) >= 2 {
		if br, err := strconv.Atoi(string(match[1])); err == nil {
			txBytes = br
		}
	}
	if match := signalRE.FindSubmatch(out); len(match) >= 2 {
		if sig, err := strconv.Atoi(string(match[1])); err == nil {
			signal = sig
		}
	}
	rxBytes, txBytes = getByteDiff(rxBytes, txBytes)
	rxAvg.Value = rxBytes
	rxAvg = rxAvg.Next()
	txAvg.Value = txBytes
	txAvg = txAvg.Next()
	rxBytes = getRollingAverage(rxAvg)
	txBytes = getRollingAverage(txAvg)
	return
}

func wiredStatus(dev string) int {
	speed, err := sysfsIntVal(filepath.Join(netSysPath, dev, "speed"))
	if err != nil {
		return 0
	}
	return speed
}

func netDevStatus(dev string) string {
	status, err := sysfsStringVal(filepath.Join(netSysPath, dev, "operstate"))
	up := err == nil && status == "up"
	if _, err = os.Stat(filepath.Join(netSysPath, dev, "wireless")); err == nil {
		ssid, rxBytes, txBytes, signal := wifiStatus(dev, up)
		return WifiFmt(dev, ssid, rxBytes, txBytes, signal, up)
	}
	speed := wiredStatus(dev)
	return WiredFmt(dev, speed, up)
}

func netStatus(devs ...string) statusFunc {
	return func() string {
		var netStats []string
		for _, dev := range devs {
			netStats = append(netStats, netDevStatus(dev))
		}
		return NetFmt(netStats)
	}
}

func batteryDevStatus(batt string) string {
	pct, err := sysfsIntVal(filepath.Join(battSysPath, batt, "capacity"))
	if err != nil {
		return Unknown
	}
	status, err := sysfsStringVal(filepath.Join(battSysPath, batt, "status"))
	if err != nil {
		return Unknown
	}
	return BatteryDevFmt(pct, status)
}

func batteryStatus(batts ...string) statusFunc {
	return func() string {
		var battStats []string
		for _, batt := range batts {
			battStats = append(battStats, batteryDevStatus(batt))
		}
		return BatteryFmt(battStats)
	}
}

func alsaAudioStatus(args ...string) statusFunc {
	args = append(args, []string{"get", "Master"}...)
	return func() string {
		out, err := exec.Command("amixer", args...).Output()
		if err != nil {
			return Unknown
		}
		match := amixerRE.FindSubmatch(out)
		if len(match) < 3 {
			return Unknown
		}
		vol, err := strconv.Atoi(string(match[1]))
		if err != nil {
			return Unknown
		}
		muted := (string(match[2]) == "off")
		return AudioFmt(vol, muted)
	}
}

func pulseAudioStatus(args ...string) statusFunc {
	volargs := append(args, []string{"--get-volume"}...)
	muteargs := append(args, []string{"--get-mute"}...)
	return func() string {
		out, err := exec.Command("pulsemixer", muteargs...).Output()
		if err != nil {
			return Unknown
		}
		muted := false
		if strings.TrimSpace(string(out)) == "1" {
			muted = true
		}
		out, err = exec.Command("pulsemixer", volargs...).Output()
		if err != nil {
			return Unknown
		}
		match := strings.Split(string(out), " ")
		if len(match) < 2 {
			return Unknown
		}
		vol, err := strconv.Atoi(match[0])
		if err != nil {
			return Unknown
		}
		return AudioFmt(vol, muted)
	}
}

func timeStatus() string {
	return TimeFmt(time.Now())
}

func status() string {
	var stats []string
	for _, item := range Items {
		stats = append(stats, item())
	}
	return StatusFmt(stats)
}

func setStatus(statusText string) {
	xproto.ChangeProperty(xconn, xproto.PropModeReplace, xroot, xproto.AtomWmName,
		xproto.AtomString, 8, uint32(len(statusText)), []byte(statusText))
}

func sysfsIntVal(path string) (int, error) {
	data, err := ioutil.ReadFile(path)
	if err != nil {
		return 0, err
	}
	val, err := strconv.Atoi(string(bytes.TrimSpace(data)))
	if err != nil {
		return 0, err
	}
	return val, nil
}

func sysfsStringVal(path string) (string, error) {
	data, err := ioutil.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(bytes.TrimSpace(data)), nil
}

func filterEmpty(strings []string) []string {
	filtStrings := strings[:0]
	for _, str := range strings {
		if str != "" {
			filtStrings = append(filtStrings, str)
		}
	}
	return filtStrings
}

func run() {
	setStatus(status())
	defer setStatus("") // cleanup
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGUSR1)
	update := time.Tick(UpdatePeriod)
	for {
		select {
		case sig := <-sigs:
			switch sig {
			case syscall.SIGUSR1:
				setStatus(status())
			default:
				return
			}
		case <-update:
			setStatus(status())
		}
	}
}

func main() {
	var err error
	xconn, err = xgb.NewConn()
	if err != nil {
		log.Fatal(err)
	}
	defer xconn.Close()
	xroot = xproto.Setup(xconn).DefaultScreen(xconn).Root
	rxAvg = ring.New(5)
	txAvg = ring.New(5)
	run()
}
