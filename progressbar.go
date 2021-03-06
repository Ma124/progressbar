package progressbar

import (
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/mitchellh/colorstring"
)

// ProgressBar is a thread-safe, simple
// progress bar
type ProgressBar struct {
	state  state
	config config

	lock sync.Mutex
}

// State is the basic properties of the bar
type State struct {
	CurrentPercent float64
	CurrentBytes   float64
	MaxBytes       int
	SecondsSince   float64
	SecondsLeft    float64
	KBsPerSecond   float64
}

type state struct {
	currentNum        int
	currentPercent    int
	lastPercent       int
	currentSaucerSize int

	lastShown time.Time
	startTime time.Time

	maxLineWidth int
	currentBytes float64
}

type config struct {
	max                  int // max number of the counter
	width                int
	writer               io.Writer
	theme                Theme
	renderWithBlankState bool
	description          string
	// whether the output is expected to contain color codes
	colorCodes bool
	maxBytes   int
	// show the iterations per second
	showIterationsPerSecond bool
	showIterationsCount     bool

	// minimum time to wait in between updates
	throttleDuration time.Duration
}

// Theme defines the elements of the bar
type Theme struct {
	Saucer        string
	SaucerHead    string
	SaucerPadding string
	BarStart      string
	BarEnd        string
}

// Option is the type all options need to adhere to
type Option func(p *ProgressBar)

// OptionSetWidth sets the width of the bar
func OptionSetWidth(s int) Option {
	return func(p *ProgressBar) {
		p.config.width = s
	}
}

// OptionSetTheme sets the elements the bar is constructed of
func OptionSetTheme(t Theme) Option {
	return func(p *ProgressBar) {
		p.config.theme = t
	}
}

// OptionSetWriter sets the output writer (defaults to os.StdOut)
func OptionSetWriter(w io.Writer) Option {
	return func(p *ProgressBar) {
		p.config.writer = w
	}
}

// OptionSetRenderBlankState sets whether or not to render a 0% bar on construction
func OptionSetRenderBlankState(r bool) Option {
	return func(p *ProgressBar) {
		p.config.renderWithBlankState = r
	}
}

// OptionSetDescription sets the description of the bar to render in front of it
func OptionSetDescription(description string) Option {
	return func(p *ProgressBar) {
		p.config.description = description
	}
}

// OptionEnableColorCodes enables or disables support for color codes
// using mitchellh/colorstring
func OptionEnableColorCodes(colorCodes bool) Option {
	return func(p *ProgressBar) {
		p.config.colorCodes = colorCodes
	}
}

// OptionSetBytes will also print the bytes/second
func OptionSetBytes(maxBytes int) Option {
	return func(p *ProgressBar) {
		p.config.maxBytes = maxBytes
	}
}

// OptionShowCount will also print current count out of total
func OptionShowCount() Option {
	return func(p *ProgressBar) {
		p.config.showIterationsCount = true
	}
}

// OptionShowIts will also print the iterations/second
func OptionShowIts() Option {
	return func(p *ProgressBar) {
		p.config.showIterationsPerSecond = true
	}
}

// OptionThrottle will wait the specified duration before updating again. The default
// duration is 0 seconds.
func OptionThrottle(duration time.Duration) Option {
	return func(p *ProgressBar) {
		p.config.throttleDuration = duration
	}
}

var defaultTheme = Theme{Saucer: "█", SaucerPadding: " ", BarStart: "|", BarEnd: "|"}

// NewOptions constructs a new instance of ProgressBar, with any options you specify
func NewOptions(max int, options ...Option) *ProgressBar {
	b := ProgressBar{
		state: getBlankState(),
		config: config{
			writer:           os.Stdout,
			theme:            defaultTheme,
			width:            40,
			max:              max,
			throttleDuration: 0 * time.Nanosecond,
		},
	}

	for _, o := range options {
		o(&b)
	}

	if b.config.renderWithBlankState {
		b.RenderBlank()
	}

	return &b
}

func getBlankState() state {
	now := time.Now()
	return state{
		startTime: now,
		lastShown: now,
	}
}

// New returns a new ProgressBar
// with the specified maximum
func New(max int) *ProgressBar {
	return NewOptions(max)
}

// RenderBlank renders the current bar state, you can use this to render a 0% state
func (p *ProgressBar) RenderBlank() error {
	return p.render()
}

// Reset will reset the clock that is used
// to calculate current time and the time left.
func (p *ProgressBar) Reset() {
	p.lock.Lock()
	defer p.lock.Unlock()

	p.state = getBlankState()
}

// Finish will fill the bar to full
func (p *ProgressBar) Finish() error {
	p.lock.Lock()
	p.state.currentNum = p.config.max
	p.lock.Unlock()
	return p.Add(0)
}

// Add with increase the current count on the progress bar
func (p *ProgressBar) Set(num int) error {
	p.lock.Lock()
	defer p.lock.Unlock()

	if p.config.max == 0 {
		return errors.New("max must be greater than 0")
	}
	p.state.currentNum = num
	percent := float64(p.state.currentNum) / float64(p.config.max)
	p.state.currentSaucerSize = int(percent * float64(p.config.width))
	p.state.currentPercent = int(percent * 100)
	updateBar := p.state.currentPercent != p.state.lastPercent && p.state.currentPercent > 0

	p.state.currentBytes = float64(percent) * float64(p.config.maxBytes)
	p.state.lastPercent = p.state.currentPercent
	if p.state.currentNum > p.config.max {
		return errors.New("current number exceeds max")
	}

	// always update if show bytes/second or its/second
	if updateBar || p.config.showIterationsPerSecond || p.config.maxBytes > 0 {
		return p.render()
	}

	return nil
}

func (p *ProgressBar) Add(num int) error {
	return p.Set(p.state.currentNum + num)
}

// Clear erases the progress bar from the current line
func (p *ProgressBar) Clear() error {
	return clearProgressBar(p.config, p.state)
}

// render renders the progress bar, updating the maximum
// rendered line width. this function is not thread-safe,
// so it must be called with an acquired lock.
func (p *ProgressBar) render() error {
	// make sure that the rendering is not happening too quickly
	// but always show if the currentNum reaches the max
	if time.Since(p.state.lastShown).Nanoseconds() < p.config.throttleDuration.Nanoseconds() &&
		p.state.currentNum < p.config.max {
		return nil
	}

	// first, clear the existing progress bar
	err := clearProgressBar(p.config, p.state)
	if err != nil {
		return err
	}

	// then, re-render the current progress bar
	w, err := renderProgressBar(p.config, p.state)
	if err != nil {
		return err
	}

	if w > p.state.maxLineWidth {
		p.state.maxLineWidth = w
	}

	p.state.lastShown = time.Now()

	return nil
}

// State returns the current state
func (p *ProgressBar) State() State {
	p.lock.Lock()
	defer p.lock.Unlock()
	s := State{}
	s.CurrentPercent = float64(p.state.currentNum) / float64(p.config.max)
	s.CurrentBytes = p.state.currentBytes
	s.MaxBytes = p.config.maxBytes
	s.SecondsSince = time.Since(p.state.startTime).Seconds()
	if p.state.currentNum > 0 {
		s.SecondsLeft = s.SecondsSince / float64(p.state.currentNum) * (float64(p.config.max) - float64(p.state.currentNum))
	}
	s.KBsPerSecond = float64(p.state.currentBytes) / 1000.0 / s.SecondsSince
	return s
}

// regex matching ansi escape codes
var ansiRegex = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]`)

func renderProgressBar(c config, s state) (int, error) {
	var leftTime float64
	if s.currentNum > 0 {
		leftTime = time.Since(s.startTime).Seconds() / float64(s.currentNum) * (float64(c.max) - float64(s.currentNum))
	}

	var saucer string
	if s.currentSaucerSize > 0 {
		saucer = strings.Repeat(c.theme.Saucer, s.currentSaucerSize-1)
		saucerHead := c.theme.SaucerHead
		if saucerHead == "" || s.currentSaucerSize == c.width {
			// use the saucer for the saucer head if it hasn't been set
			// to preserve backwards compatibility
			saucerHead = c.theme.Saucer
		}
		saucer += saucerHead
	}

	// add on bytes string if max bytes option was set
	kbPerSecond := float64(s.currentBytes) / 1000.0 / time.Since(s.startTime).Seconds()
	bytesString := ""
	if kbPerSecond > 1000.0 {
		bytesString = fmt.Sprintf("(%2.1f MB/s)", kbPerSecond/1000.0)
	} else if kbPerSecond > 0 {
		bytesString = fmt.Sprintf("(%2.1f kB/s)", kbPerSecond)
	}

	if c.showIterationsPerSecond && !c.showIterationsCount {
		// replace bytesString if used
		bytesString = fmt.Sprintf("(%2.0f it/s)", float64(s.currentNum)/time.Since(s.startTime).Seconds())
	} else if !c.showIterationsPerSecond && c.showIterationsCount {
		bytesString = fmt.Sprintf("(%d/%d)", s.currentNum, c.max)
	} else if c.showIterationsPerSecond && c.showIterationsCount {
		bytesString = fmt.Sprintf("(%d/%d, %2.0f it/s)", s.currentNum, c.max, float64(s.currentNum)/time.Since(s.startTime).Seconds())
	}

	str := fmt.Sprintf("\r%s%4d%% %s%s%s%s %s [%s:%s]",
		c.description,
		s.currentPercent,
		c.theme.BarStart,
		saucer,
		strings.Repeat(c.theme.SaucerPadding, c.width-s.currentSaucerSize),
		c.theme.BarEnd,
		bytesString,
		(time.Duration(time.Since(s.startTime).Seconds()) * time.Second).String(),
		(time.Duration(leftTime) * time.Second).String(),
	)

	if c.colorCodes {
		// convert any color codes in the progress bar into the respective ANSI codes
		str = colorstring.Color(str)
	}

	// the width of the string, if printed to the console
	// does not include the carriage return character
	cleanString := strings.Replace(str, "\r", "", -1)

	if c.colorCodes {
		// the ANSI codes for the colors do not take up space in the console output,
		// so they do not count towards the output string width
		cleanString = ansiRegex.ReplaceAllString(cleanString, "")
	}

	// get the amount of runes in the string instead of the
	// character count of the string, as some runes span multiple characters.
	// see https://stackoverflow.com/a/12668840/2733724
	stringWidth := len([]rune(cleanString))

	return stringWidth, writeString(c, str)
}

func clearProgressBar(c config, s state) error {
	// fill the current line with enough spaces
	// to overwrite the progress bar and jump
	// back to the beginning of the line
	str := fmt.Sprintf("\r%s\r", strings.Repeat(" ", s.maxLineWidth))
	return writeString(c, str)
}

func writeString(c config, str string) error {
	if _, err := io.WriteString(c.writer, str); err != nil {
		return err
	}

	if f, ok := c.writer.(*os.File); ok {
		// ignore any errors in Sync(), as stdout
		// can't be synced on some operating systems
		// like Debian 9 (Stretch)
		f.Sync()
	}

	return nil
}
