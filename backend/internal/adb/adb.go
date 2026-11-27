package adb

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
)

// Device describes a device line reported by `adb devices -l`.
type Device struct {
	Serial string
	State  string
	Model  string
}

// Client executes adb commands using a resolved adb binary path.
type Client struct {
	Path string
}

type findDeps struct {
	goos       string
	getenv     func(string) string
	lookPath   func(string) (string, error)
	executable func() (string, error)
	stat       func(string) (os.FileInfo, error)
	glob       func(string) ([]string, error)
}

func defaultFindDeps() findDeps {
	return findDeps{
		goos:       runtime.GOOS,
		getenv:     os.Getenv,
		lookPath:   exec.LookPath,
		executable: os.Executable,
		stat:       os.Stat,
		glob:       filepath.Glob,
	}
}

// Find locates adb.exe (or adb on non-Windows) using the MirrPane search order:
//  1. MIRRPANE_ADB env var (authoritative — a bad path returns an error)
//  2. PATH
//  3. %LOCALAPPDATA%\Microsoft\WinGet\Packages\Google.PlatformTools*\platform-tools\adb.exe
//  4. %LOCALAPPDATA%\Android\Sdk\platform-tools\adb.exe
//  5. %USERPROFILE%\AppData\Local\Android\Sdk\platform-tools\adb.exe
//  6. <exeDir>\platform-tools\adb.exe
//  7. C:\platform-tools\adb.exe
func Find() (string, error) {
	return findWith(defaultFindDeps())
}

func findWith(d findDeps) (string, error) {
	isRegular := func(p string) bool {
		if p == "" {
			return false
		}
		info, err := d.stat(p)
		return err == nil && !info.IsDir()
	}

	if envPath := strings.TrimSpace(d.getenv("MIRRPANE_ADB")); envPath != "" {
		info, err := d.stat(envPath)
		if err != nil {
			return "", fmt.Errorf("adb: MIRRPANE_ADB=%q: %w", envPath, err)
		}
		if info.IsDir() {
			return "", fmt.Errorf("adb: MIRRPANE_ADB=%q is a directory", envPath)
		}
		return filepath.Clean(envPath), nil
	}

	binName := "adb"
	if d.goos == "windows" {
		binName = "adb.exe"
	}

	if p, err := d.lookPath(binName); err == nil && isRegular(p) {
		return p, nil
	}

	var candidates []string
	localAppData := strings.TrimSpace(d.getenv("LOCALAPPDATA"))
	if localAppData != "" {
		pattern := filepath.Join(localAppData, "Microsoft", "WinGet", "Packages", "Google.PlatformTools*", "platform-tools", binName)
		if matches, err := d.glob(pattern); err == nil && len(matches) > 0 {
			sort.Strings(matches)
			candidates = append(candidates, matches...)
		}
		candidates = append(candidates, filepath.Join(localAppData, "Android", "Sdk", "platform-tools", binName))
	}

	if userProfile := strings.TrimSpace(d.getenv("USERPROFILE")); userProfile != "" {
		candidates = append(candidates, filepath.Join(userProfile, "AppData", "Local", "Android", "Sdk", "platform-tools", binName))
	}

	if exePath, err := d.executable(); err == nil && exePath != "" {
		candidates = append(candidates, filepath.Join(filepath.Dir(exePath), "platform-tools", binName))
	}

	if d.goos == "windows" {
		candidates = append(candidates, `C:\platform-tools\adb.exe`)
	}

	for _, c := range candidates {
		if isRegular(c) {
			return filepath.Clean(c), nil
		}
	}

	return "", errors.New("adb: executable not found; set MIRRPANE_ADB or install Android platform-tools")
}

// New creates a Client for the given adb executable path, or calls Find if path is empty.
func New(path string) (*Client, error) {
	if strings.TrimSpace(path) == "" {
		found, err := Find()
		if err != nil {
			return nil, err
		}
		return &Client{Path: found}, nil
	}

	info, err := os.Stat(path)
	if err != nil {
		// Allow bare binary name in PATH (e.g. "adb" or "adb.exe").
		if !filepath.IsAbs(path) && !strings.ContainsAny(path, `/\`) {
			if resolved, lpErr := exec.LookPath(path); lpErr == nil {
				return &Client{Path: resolved}, nil
			}
		}
		return nil, fmt.Errorf("adb: invalid executable %q: %w", path, err)
	}
	if info.IsDir() {
		return nil, fmt.Errorf("adb: %q is a directory", path)
	}
	return &Client{Path: path}, nil
}

// Command returns an *exec.Cmd configured with hidden console windows on Windows.
func (c *Client) Command(ctx context.Context, args ...string) *exec.Cmd {
	if ctx == nil {
		ctx = context.Background()
	}
	cmd := exec.CommandContext(ctx, c.Path, args...)
	hideWindow(cmd)
	return cmd
}

func (c *Client) run(ctx context.Context, args ...string) (stdout, stderr string, err error) {
	cmd := c.Command(ctx, args...)
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err = cmd.Run()
	return outBuf.String(), errBuf.String(), err
}

func formatCmdError(op string, err error, stdout, stderr string) error {
	errMsg := strings.TrimSpace(stderr)
	outMsg := strings.TrimSpace(stdout)
	switch {
	case err != nil && errMsg != "" && outMsg != "":
		return fmt.Errorf("%s: %w (stderr: %s; stdout: %s)", op, err, errMsg, outMsg)
	case err != nil && errMsg != "":
		return fmt.Errorf("%s: %w: %s", op, err, errMsg)
	case err != nil && outMsg != "":
		return fmt.Errorf("%s: %w: %s", op, err, outMsg)
	case err != nil:
		return fmt.Errorf("%s: %w", op, err)
	case errMsg != "" && outMsg != "":
		return fmt.Errorf("%s: %s (stdout: %s)", op, errMsg, outMsg)
	case errMsg != "":
		return fmt.Errorf("%s: %s", op, errMsg)
	case outMsg != "":
		return fmt.Errorf("%s: %s", op, outMsg)
	default:
		return fmt.Errorf("%s failed", op)
	}
}

// Devices runs `adb devices -l` and parses the attached device list.
func (c *Client) Devices(ctx context.Context) ([]Device, error) {
	stdout, stderr, err := c.run(ctx, "devices", "-l")
	if err != nil {
		return nil, formatCmdError("adb devices -l", err, stdout, stderr)
	}
	devices, err := ParseDevicesOutput(stdout)
	if err != nil {
		if msg := strings.TrimSpace(stderr); msg != "" {
			return nil, fmt.Errorf("%w (stderr: %s)", err, msg)
		}
		return nil, err
	}
	return devices, nil
}

// Connect connects to a wireless/TCP ADB endpoint (host:port).
// "already connected" is treated as success.
func (c *Client) Connect(ctx context.Context, address string) error {
	address = strings.TrimSpace(address)
	if address == "" {
		return errors.New("adb connect: empty address")
	}
	stdout, stderr, err := c.run(ctx, "connect", address)
	if err != nil {
		return formatCmdError("adb connect "+address, err, stdout, stderr)
	}

	// adb connect often exits 0 even when connection fails, reporting errors on stdout.
	lines := nonBannerLines(stdout)
	for i := len(lines) - 1; i >= 0; i-- {
		lower := strings.ToLower(lines[i])
		if strings.HasPrefix(lower, "connected") || strings.HasPrefix(lower, "already connected") {
			return nil
		}
	}
	return formatCmdError("adb connect "+address, nil, stdout, stderr)
}

// Pair pairs with an Android wireless debugging endpoint using the pairing code.
func (c *Client) Pair(ctx context.Context, address, code string) error {
	address = strings.TrimSpace(address)
	code = strings.TrimSpace(code)
	if address == "" {
		return errors.New("adb pair: empty address")
	}
	if code == "" {
		return errors.New("adb pair: empty pairing code")
	}

	stdout, stderr, err := c.run(ctx, "pair", address, code)
	if err != nil {
		return formatCmdError("adb pair "+address, err, stdout, stderr)
	}

	combined := strings.ToLower(stdout + "\n" + stderr)
	if strings.Contains(combined, "successfully paired") {
		return nil
	}
	if strings.Contains(combined, "failed") || strings.Contains(combined, "error:") || strings.TrimSpace(stderr) != "" {
		return formatCmdError("adb pair "+address, nil, stdout, stderr)
	}
	return nil
}

// Disconnect disconnects a wireless/TCP ADB endpoint.
func (c *Client) Disconnect(ctx context.Context, address string) error {
	address = strings.TrimSpace(address)
	if address == "" {
		return errors.New("adb disconnect: empty address")
	}
	stdout, stderr, err := c.run(ctx, "disconnect", address)
	if err != nil {
		return formatCmdError("adb disconnect "+address, err, stdout, stderr)
	}
	combined := strings.ToLower(stdout + "\n" + stderr)
	if strings.Contains(combined, "error:") {
		return formatCmdError("adb disconnect "+address, nil, stdout, stderr)
	}
	return nil
}

// Push copies localPath to remotePath on the device identified by serial.
func (c *Client) Push(ctx context.Context, serial, localPath, remotePath string) error {
	if serial == "" {
		return errors.New("adb push: empty serial")
	}
	stdout, stderr, err := c.run(ctx, "-s", serial, "push", localPath, remotePath)
	if err != nil {
		return formatCmdError("adb push", err, stdout, stderr)
	}
	return nil
}

// Forward sets up `adb -s <serial> forward <local> <remote>`.
// When local is "tcp:0", adb prints the allocated port on stdout and Forward returns it.
func (c *Client) Forward(ctx context.Context, serial, local, remote string) (int, error) {
	if serial == "" {
		return 0, errors.New("adb forward: empty serial")
	}
	stdout, stderr, err := c.run(ctx, "-s", serial, "forward", local, remote)
	if err != nil {
		return 0, formatCmdError("adb forward", err, stdout, stderr)
	}

	out := strings.TrimSpace(stdout)
	if out != "" {
		// Ignore any daemon banner lines and parse the last line as the port.
		lines := nonBannerLines(stdout)
		if len(lines) > 0 {
			if port, convErr := strconv.Atoi(strings.TrimSpace(lines[len(lines)-1])); convErr == nil && port > 0 {
				return port, nil
			}
		}
	}

	if strings.HasPrefix(local, "tcp:") {
		if port, convErr := strconv.Atoi(strings.TrimPrefix(local, "tcp:")); convErr == nil && port > 0 {
			return port, nil
		}
	}

	return 0, formatCmdError("adb forward (missing port in output)", nil, stdout, stderr)
}

// ForwardRemove removes a local forward spec (e.g. "tcp:12345") for serial.
func (c *Client) ForwardRemove(ctx context.Context, serial, local string) error {
	if serial == "" {
		return errors.New("adb forward --remove: empty serial")
	}
	stdout, stderr, err := c.run(ctx, "-s", serial, "forward", "--remove", local)
	if err != nil {
		return formatCmdError("adb forward --remove "+local, err, stdout, stderr)
	}
	return nil
}

// ForwardEntry represents an entry from `adb forward --list`.
type ForwardEntry struct {
	Serial string
	Local  string
	Remote string
}

// ForwardList lists active forwards for serial (or all devices if serial is empty).
func (c *Client) ForwardList(ctx context.Context, serial string) ([]ForwardEntry, error) {
	var args []string
	if serial != "" {
		args = []string{"-s", serial, "forward", "--list"}
	} else {
		args = []string{"forward", "--list"}
	}
	stdout, stderr, err := c.run(ctx, args...)
	if err != nil {
		return nil, formatCmdError("adb forward --list", err, stdout, stderr)
	}
	var entries []ForwardEntry
	for _, line := range nonBannerLines(stdout) {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		if serial != "" && fields[0] != serial {
			continue
		}
		entries = append(entries, ForwardEntry{
			Serial: fields[0],
			Local:  fields[1],
			Remote: fields[2],
		})
	}
	return entries, nil
}

func nonBannerLines(s string) []string {
	rawLines := strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
	var out []string
	for _, line := range rawLines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "*") || strings.HasPrefix(trimmed, "adb server") {
			continue
		}
		out = append(out, trimmed)
	}
	return out
}

var nextFieldPattern = regexp.MustCompile(`\s+(?:usb|product|model|device|transport_id):`)

// ParseDevicesOutput parses the stdout of `adb devices -l`.
func ParseDevicesOutput(output string) ([]Device, error) {
	normalized := strings.ReplaceAll(output, "\r\n", "\n")
	lines := strings.Split(normalized, "\n")

	headerFound := false
	var devices []Device
	for _, rawLine := range lines {
		line := strings.TrimRight(rawLine, "\r \t")
		if !headerFound {
			if strings.HasPrefix(line, "List of devices attached") {
				headerFound = true
			}
			continue
		}

		if line == "" || strings.HasPrefix(line, "*") || strings.HasPrefix(line, "adb server") {
			continue
		}

		dev, ok := parseDeviceLine(line)
		if ok {
			devices = append(devices, dev)
		}
	}

	if !headerFound {
		return nil, errors.New("adb devices -l: missing 'List of devices attached' header")
	}
	return devices, nil
}

func isDeviceState(tok string) bool {
	switch tok {
	case "device", "unauthorized", "offline", "bootloader",
		"host", "recovery", "rescue", "sideload",
		"authorizing", "connecting", "detached":
		return true
	default:
		return false
	}
}

func parseDeviceLine(line string) (Device, bool) {
	// Scan tokens from the right to locate the device state token, matching
	// scrcpy's adb_parser.c so serials containing spaces still parse cleanly,
	// while also preserving model values that contain spaces.
	stateStart, stateEnd := -1, -1
	pos := len(line)
	for {
		for pos > 0 && (line[pos-1] == ' ' || line[pos-1] == '\t') {
			pos--
		}
		end := pos
		for pos > 0 && line[pos-1] != ' ' && line[pos-1] != '\t' {
			pos--
		}
		start := pos
		if start == end {
			break
		}
		tok := line[start:end]
		if isDeviceState(tok) {
			stateStart = start
			stateEnd = end
			break
		}
	}

	if stateStart == -1 {
		return Device{}, false
	}

	serial := strings.TrimRight(line[:stateStart], " \t")
	if serial == "" {
		return Device{}, false
	}
	state := line[stateStart:stateEnd]
	rest := line[stateEnd:]
	model := extractModelField(rest)

	return Device{
		Serial: serial,
		State:  state,
		Model:  model,
	}, true
}

func extractModelField(rest string) string {
	idx := -1
	for i := 0; i < len(rest); i++ {
		if rest[i] != 'm' {
			continue
		}
		if i > 0 && rest[i-1] != ' ' && rest[i-1] != '\t' {
			continue
		}
		if strings.HasPrefix(rest[i:], "model:") {
			idx = i + len("model:")
			break
		}
	}
	if idx == -1 {
		return ""
	}
	tail := rest[idx:]
	if loc := nextFieldPattern.FindStringIndex(tail); loc != nil {
		tail = tail[:loc[0]]
	}
	return strings.TrimSpace(tail)
}
