package main

import (
	"time"
)

const (
	Delimiter    = " │ "
	Unknown      = "?"
	UpdatePeriod = 5 * time.Second
)

var Items = []statusFunc{
	netStatus("wlp3s0", "enp0s25"),
	batteryStatus("BAT0", "BAT1"),
	alsaAudioStatus("-M"),
	// defaults to non-padding and 12-hour time
	timeStatus("1/2/2006", "3:04"),
}
