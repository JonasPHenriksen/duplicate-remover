package main

import (
	"encoding/base64"
	"errors"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"unicode/utf16"
)

// hasDisplay reports whether there is a screen to show a web browser on: on
// Windows and macOS unless logged in over SSH, elsewhere when X11 or Wayland
// is running.
func hasDisplay() bool {
	if os.Getenv("SSH_CONNECTION") != "" {
		return false
	}
	switch runtime.GOOS {
	case "windows", "darwin":
		return true
	}
	return os.Getenv("DISPLAY") != "" || os.Getenv("WAYLAND_DISPLAY") != ""
}

// openBrowser shows url in the default web browser. It and the two below are
// variables so that tests never open real windows.
var openBrowser = func(url string) error {
	switch runtime.GOOS {
	case "windows":
		return launch("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		return launch("open", url)
	}
	if err := launch("xdg-open", url); err != nil {
		return launch("gio", "open", url)
	}
	return nil
}

// revealFolder opens a folder in Finder, Explorer or the desktop's file manager.
var revealFolder = func(dir string) error {
	switch runtime.GOOS {
	case "windows":
		return launch("explorer", dir)
	case "darwin":
		return launch("open", dir)
	}
	return launch("xdg-open", dir)
}

// launch starts a program without waiting for it to finish.
func launch(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait()
	return nil
}

var errNoPicker = errors.New("this computer has no folder picker")

// pickFolder shows the system's folder picker with title on it, and returns the
// chosen folder, or "" when it was cancelled. On Linux it opens in start, if
// that is not "".
var pickFolder = func(title, start string) (string, error) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("osascript", "-e", "activate",
			"-e", `POSIX path of (choose folder with prompt "`+appleScriptQuote.Replace(title)+`")`)
	case "windows":
		cmd = exec.Command("powershell", "-NoProfile", "-STA", "-EncodedCommand", encodePowerShell(windowsPicker(title)))
	default:
		if zenity, err := exec.LookPath("zenity"); err == nil {
			args := []string{"--file-selection", "--directory", "--title=" + title}
			if start != "" {
				args = append(args, "--filename="+start+"/") // the slash opens it, rather than its parent
			}
			cmd = exec.Command(zenity, args...)
		} else if kdialog, err := exec.LookPath("kdialog"); err == nil {
			if start == "" {
				start, _ = os.UserHomeDir()
			}
			cmd = exec.Command(kdialog, "--getexistingdirectory", start, "--title", title)
		} else {
			return "", errNoPicker
		}
	}
	out, err := cmd.Output()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return "", nil // cancelled
	}
	if err != nil {
		return "", errNoPicker
	}
	return strings.TrimRight(string(out), "\r\n"), nil
}

// appleScriptQuote escapes text for a string in double quotes in AppleScript.
var appleScriptQuote = strings.NewReplacer(`\`, `\\`, `"`, `\"`)

// windowsPicker shows Windows' folder picker, with title on it, on top of the
// browser and prints the chosen folder as UTF-8.
func windowsPicker(title string) string {
	return `
Add-Type -AssemblyName System.Windows.Forms
[Console]::OutputEncoding = [Text.Encoding]::UTF8
$owner = New-Object System.Windows.Forms.Form -Property @{ TopMost = $true }
$dialog = New-Object System.Windows.Forms.FolderBrowserDialog
$dialog.Description = '` + strings.ReplaceAll(title, "'", "''") + `'
$dialog.ShowNewFolderButton = $false
if ($dialog.ShowDialog($owner) -eq 'OK') { [Console]::Out.Write($dialog.SelectedPath) }
`
}

// encodePowerShell turns a script into what powershell -EncodedCommand takes,
// base64 of UTF-16LE, which sidesteps every quoting problem.
func encodePowerShell(script string) string {
	var b []byte
	for _, u := range utf16.Encode([]rune(script)) {
		b = append(b, byte(u), byte(u>>8))
	}
	return base64.StdEncoding.EncodeToString(b)
}
