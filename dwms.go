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

type statusFunc func() []string

const (
	battSysPath = "/sys/class/power_supply"
	netSysPath  = "/sys/class/net"
)

var (
	ssidRE      = regexp.MustCompile(`SSID:\s+(.*)`)
	signalRE    = regexp.MustCompile(`signal:\s+(-\d+)`)
	amixerRE    = regexp.MustCompile(`\[(\d+)%\].*\[([.\w]+)\]`)
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

func wifiFmt(dev, ssid string, rxBytes, txBytes, signal int, up bool) []string {
	if !up {
		return []string{}
	}
	rx := formatBytes(rxBytes)
	tx := formatBytes(txBytes)
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
	return strs
}

func wiredFmt(dev string, speed int, up bool) []string {
	if !up {
		return []string{}
	}
	return []string{"[=" + strconv.Itoa(speed)}
}

func netFmt(devs []string) []string {
	return filterEmpty(devs)
}

func batteryDevFmt(pct int, state string) string {
	spct := strconv.Itoa(pct)
	smoji := map[string]string{"Charging": "🔌", "Full": "🔌", "Discharging": "🔋"}[state]
	return format{smoji, spct, 3}.String()
}

func audioFmt(vol int, muted bool) string {
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

func timeFmt(t time.Time, dateFormat string, timeFormat string) []string {
	// shift from last half-hour to closest half-hour
	offsetTime := t.Add(time.Minute * 15)
	// get clock row
	hour := offsetTime.Hour() % 12
	// get clock col
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

	dateFmted := t.Format(dateFormat)
	timeFmted := t.Format(timeFormat)

	// size to the largest possible value to prevent the size from changing after updates
	large := time.Date(2006, 10, 11, 12, 13, 15, 500, t.Location())
	largeDate := large.Format(dateFormat)
	largeTime := large.Format(timeFormat)

	dateResult := format{"📅", dateFmted, len(largeDate)}.String()
	timeResult := format{clockEmoji, timeFmted, len(largeTime)}.String()
	return []string{dateResult, timeResult}
}

func statusFmt(stats []string) string {
	return " " + strings.Join(filterEmpty(stats), Delimiter) + " "
}

func getByteDiff(rxBytes, txBytes int) (int, int) {
	defer func() {
		lastRxBytes, lastTxBytes = rxBytes, txBytes
	}()
	if rxBytes < lastRxBytes || txBytes < lastTxBytes {
		return 0, 0
	}
	rx := 0
	if lastRxBytes >= 0 {
		rx = rxBytes - lastRxBytes
	}
	tx := 0
	if lastTxBytes >= 0 {
		tx = txBytes - lastTxBytes
	}
	return rx, tx
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
	// Bytes per period
	bpp := float64(sum) / float64(count)
	secs := float64(UpdatePeriod) / float64(time.Second)
	// Bytes per second
	return int(bpp / secs)
}

func wifiStatus(dev string, up bool) (ssid string, rxBytes int, txBytes int, signal int) {
	if !up {
		rxAvg = ring.New(5)
		txAvg = ring.New(5)
		lastRxBytes = -1
		lastTxBytes = -1
		return
	}
	out, err := exec.Command("iw", "dev", dev, "link").Output()
	if err != nil {
		return
	}
	if match := ssidRE.FindSubmatch(out); len(match) >= 2 {
		ssid = string(match[1])
	}
	rxBytes, err = sysfsIntVal(filepath.Join(netSysPath, dev, "statistics", "rx_bytes"))
	if err != nil {
		rxBytes = -1
	}
	txBytes, err = sysfsIntVal(filepath.Join(netSysPath, dev, "statistics", "tx_bytes"))
	if err != nil {
		txBytes = -1
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

func netDevStatus(dev string) []string {
	status, err := sysfsStringVal(filepath.Join(netSysPath, dev, "operstate"))
	up := err == nil && status == "up"
	if _, err = os.Stat(filepath.Join(netSysPath, dev, "wireless")); err == nil {
		ssid, rxBytes, txBytes, signal := wifiStatus(dev, up)
		return wifiFmt(dev, ssid, rxBytes, txBytes, signal, up)
	}
	speed := wiredStatus(dev)
	return wiredFmt(dev, speed, up)
}

func netStatus(devs ...string) statusFunc {
	return func() []string {
		var netStats []string
		for _, dev := range devs {
			netStats = append(netStats, netDevStatus(dev)...)
		}
		return netFmt(netStats)
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
	return batteryDevFmt(pct, status)
}

func batteryStatus(batts ...string) statusFunc {
	return func() []string {
		var battStats []string
		for _, batt := range batts {
			battStats = append(battStats, batteryDevStatus(batt))
		}
		return battStats
	}
}

func alsaAudioStatus(args ...string) statusFunc {
	args = append(args, []string{"get", "Master"}...)
	return func() []string {
		out, err := exec.Command("amixer", args...).Output()
		if err != nil {
			return []string{Unknown}
		}
		match := amixerRE.FindSubmatch(out)
		if len(match) < 3 {
			return []string{Unknown}
		}
		vol, err := strconv.Atoi(string(match[1]))
		if err != nil {
			return []string{Unknown}
		}
		muted := (string(match[2]) == "off")
		return []string{audioFmt(vol, muted)}
	}
}

func pulseAudioStatus(args ...string) statusFunc {
	volargs := append(args, []string{"--get-volume"}...)
	muteargs := append(args, []string{"--get-mute"}...)
	return func() []string {
		out, err := exec.Command("pulsemixer", muteargs...).Output()
		if err != nil {
			return []string{Unknown}
		}
		muted := false
		if strings.TrimSpace(string(out)) == "1" {
			muted = true
		}
		out, err = exec.Command("pulsemixer", volargs...).Output()
		if err != nil {
			return []string{Unknown}
		}
		match := strings.Split(string(out), " ")
		if len(match) < 2 {
			return []string{Unknown}
		}
		vol, err := strconv.Atoi(match[0])
		if err != nil {
			return []string{Unknown}
		}
		return []string{audioFmt(vol, muted)}
	}
}

// Formatting represents the follow date and time:
//  Mon Jan 2 15:04:05 -0700 MST 2006
func timeStatus(dateFormat string, timeFormat string) statusFunc {
	return func() []string {
		return timeFmt(time.Now(), dateFormat, timeFormat)
	}
}

func status() string {
	var stats []string
	for _, item := range Items {
		stats = append(stats, item()...)
	}
	return statusFmt(stats)
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
	lastRxBytes = -1
	lastTxBytes = -1
	run()
}
