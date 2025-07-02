package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os/exec"
	"sort"
	"strings"
	"time"

	log "github.com/cbsinteractive/mc-log"
)

var (
	split = strings.Split
	trim  = strings.TrimSpace
)

func hastext(in string, has ...string) bool {
	for _, has := range has {
		if strings.Contains(in, has) {
			return true
		}
	}
	return false
}

type GPU struct {
	N                 int
	Name, PCI, Driver string
	Used, Total       int
}

func (g GPU) Load() float64 {
	return float64(g.Used) / float64(g.Total)
}

func queryGPU() (list []GPU) {
	out, err := exec.Command(
		"nvidia-smi",
		"--query-gpu=utilization.memory,memory.total,name,pci.bus_id,driver_version",
		"--format=csv,noheader,nounits",
	).Output()
	if err != nil {
		return nil
	}
	sc := bufio.NewScanner(bytes.NewReader(out))
	n := 0
	for sc.Scan() {
		g := GPU{}
		x := strings.ReplaceAll(sc.Text(), " ", "")
		fmt.Sscanf(x, "%d,%d", &g.Used, &g.Total)
		f := strings.Split(x, ",")
		if len(f) < 5 {
			continue
		}
		g.N = n
		g.Name = f[2]
		g.PCI = f[3]
		g.Driver = f[4]
		list = append(list, g)
		n++
	}
	sort.SliceStable(list, func(i, j int) bool {
		return list[i].Load() < list[j].Load()
	})
	return list
}

func gpuOOM(s string) (oom bool) {
	defer func() {
		if oom {
			for _, g := range queryGPU() {
				log.Warn.Add(
					"gpu_num", g.N,
					"gpu_mem_used", g.Used,
					"gpu_mem_total", g.Total,
					"gpu_name", g.Name,
					"gpu_pci", g.PCI,
					"gpu_driver", g.Driver,
				).Printf("ffmpeg-json: gpu out of memory condition")
			}
		}
	}()
	if hastext(s, "nvenc") && hastext(s, "OpenEncodeSessionEx failed") {
		return true
	}
	if hastext(s, "nvenc") && hastext(s, "out of memory") {
		return true
	}
	if hastext(s, "CUDA_ERROR_OUT_OF_MEMORY") {
		return true
	}
	if hastext(s, "CUDA_ERROR_NO_DEVICE") && len(queryGPU()) != 0 {
		return true
	}
	return false
}

var globalmsg = []string{}

func watchState(r io.Reader, state chan<- State) {
	defer close(state)
	sc := bufio.NewScanner(CRtoLF{r}) // util.go:/CRtoLF/
	s0 := State{}
	for sc.Scan() {
		// NOTE(as): HWFRAMES3
		// Self-explanitory string check. That's it.
		if hastext(sc.Text(), "Impossible to convert between the formats supported by the filter") {
			filterbug = true
		}

		if hastext(sc.Text(), "No decoder surfaces left") {
			hwframesbug = true
		}

		if gpuOOM(sc.Text()) {
			vramoverflow = true
		}

		if hastext(sc.Text(), "corrupt", "invalid", "error") {
			globalmsg = append(globalmsg, sc.Text())
			log.Error.Add("topic", "ffmpeg", "action", "alert", "subject", "error", "err", sc.Text()).Printf("")
		}

		log.Debug.F("watch: state: %v", sc.Text())
		s1 := State{}.Decode(sc.Text())
		if s1.Frame <= s0.Frame && s1.Size <= s0.Size {
			continue
		}
		state <- s1
		s0 = s1
	}
}

// State is a carriage-return delimited output line in ffmpeg
type State struct {
	Frame   int
	FPS     int
	Q       float64
	Time    Time
	Size    int
	Bitrate float64
	Dup     int
	Drop    int
	Speed   float64
}

func (s State) Fields() (kv []any) {
	return []interface{}{
		"frame", s.Frame,
		"runtime", s.Time.Duration().Seconds(),
		"size", 1024 * s.Size,
		"dup", s.Dup,
		"drop", s.Drop,
		"bps", int(1000 * s.Bitrate),
		"fps", s.FPS,
		"speed", fmt.Sprintf("%0.2f", s.Speed),
		"q", s.Q,
	}
}

// Progress returns a value between [0, 1] inclusive
func (s State) Progress(max time.Duration, frames int) float64 {
	if max != 0 {
		return s.Time.Duration().Seconds() / max.Seconds()
	}
	return float64(s.Frame) / float64(frames)
}

// Decode decodes line into a new state and returns it. The line
// must begin with "frame=" (video) or "size=" (audio, packaging)
// which is what the state line looks like in the ffmpeg output.
func (s State) Decode(line string) State {
	if !strings.HasPrefix(line, "frame=") && !strings.HasPrefix(line, "size=") {
		return s
	}
	symtab := map[string]interface{}{
		"frame":   &s.Frame,
		"fps":     &s.FPS,
		"size":    &s.Size,
		"time":    &s.Time,
		"Lsize":   &s.Size, // ffmpeg bug?
		"bitrate": &s.Bitrate,
		"dup":     &s.Dup,
		"drop":    &s.Drop,
		"q":       &s.Q,
		"speed":   &s.Speed,
	}

	// ffmpeg formatting is left-padded for numbers
	// so get rid of the equal signs and treat the input
	// as a space seperated list
	a := split(demangle(line), " ")

	// scan each keypair into the symbol table
	for i := 1; i < len(a); i += 2 {
		dst, ok := symtab[trim(a[i-1])]
		if ok {
			fmt.Sscan(trim(a[i]), dst)
		}
	}
	s.FPS *= (targetOutputs)
	s.Speed *= round100(float64(targetOutputs))
	return s
}

// demangle splits the line into space-seperated
// values, discarding equal signs from the input.
func demangle(line string) (s string) {
	sep := ""
	for _, v := range split(line, "=") {
		s += sep + trim(v)
		sep = " "
	}
	return s
}

// Time helps us parse ffmpeg log times
type Time string

func (t Time) Duration() time.Duration {
	var h, m, s float64
	fmt.Sscanf(string(t), "%f:%f:%f", &h, &m, &s)
	return floatDur(3600*h + 60*m + s)
}

// CRtoLF replaces all carriage returns with line feeds
type CRtoLF struct {
	io.Reader
}

func (c CRtoLF) Read(p []byte) (n int, err error) {
	n, err = c.Reader.Read(p)
	for i := 0; i < n; i++ {
		if p[i] == '\r' {
			p[i] = '\n'
		}
	}
	return
}
