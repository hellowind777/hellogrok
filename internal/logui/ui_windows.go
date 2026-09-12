//go:build windows

package logui

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/hellowind777/hellogrok/internal/appinfo"
	"github.com/hellowind777/hellogrok/internal/dialog"
	"github.com/hellowind777/hellogrok/internal/prefs"
)

// StatusFunc returns short title + detail text for the status panel.
type StatusFunc func() (short, detail string)

// Keep callback alive for the whole process (required by Win32).
var windowProcCallback = syscall.NewCallback(wndProc)

var (
	user32   = syscall.NewLazyDLL("user32.dll")
	kernel32 = syscall.NewLazyDLL("kernel32.dll")
	gdi32    = syscall.NewLazyDLL("gdi32.dll")

	pRegisterClassExW = user32.NewProc("RegisterClassExW")
	pCreateWindowExW  = user32.NewProc("CreateWindowExW")
	pDefWindowProcW   = user32.NewProc("DefWindowProcW")
	pShowWindow       = user32.NewProc("ShowWindow")
	pUpdateWindow     = user32.NewProc("UpdateWindow")
	pGetMessageW      = user32.NewProc("GetMessageW")
	pTranslateMessage = user32.NewProc("TranslateMessage")
	pDispatchMessageW = user32.NewProc("DispatchMessageW")
	pPostQuitMessage  = user32.NewProc("PostQuitMessage")
	pLoadCursorW      = user32.NewProc("LoadCursorW")
	pLoadImageW       = user32.NewProc("LoadImageW")
	pSendMessageW     = user32.NewProc("SendMessageW")
	pGetClientRect    = user32.NewProc("GetClientRect")
	pMoveWindow       = user32.NewProc("MoveWindow")
	pInvalidateRect   = user32.NewProc("InvalidateRect")
	pSetWindowPos     = user32.NewProc("SetWindowPos")
	pDestroyWindow    = user32.NewProc("DestroyWindow")
	pSetTimer         = user32.NewProc("SetTimer")
	pKillTimer        = user32.NewProc("KillTimer")
	pSetForeground    = user32.NewProc("SetForegroundWindow")
	pSetFocus         = user32.NewProc("SetFocus")
	pGetStockObject   = gdi32.NewProc("GetStockObject")
	pGetModuleHandleW = kernel32.NewProc("GetModuleHandleW")
	pGetLastError     = kernel32.NewProc("GetLastError")
	pRtlMoveMemory    = kernel32.NewProc("RtlMoveMemory")
	pGetSystemMetrics = user32.NewProc("GetSystemMetrics")
	pGetWindowRect    = user32.NewProc("GetWindowRect")

	classOnce   sync.Once
	className   *uint16
	classRegErr error

	openMu      sync.Mutex
	openHWND    uintptr
	building    bool
	keepAlive   = map[uintptr]*winState{}
	keepAliveMu sync.Mutex
)

const (
	wsOverlappedWindow         = 0x00CF0000
	wsChild                    = 0x40000000
	wsVisible                  = 0x10000000
	wsVScroll                  = 0x00200000
	wsHScroll                  = 0x00100000
	wsBorder                   = 0x00800000
	wsClipSiblings             = 0x04000000
	wsTabStop                  = 0x00010000
	esMultiline                = 0x0004
	esReadonly                 = 0x0800
	esAutovscroll              = 0x0040
	esAutohscroll              = 0x0080
	esWantreturn               = 0x1000
	ssCenterImage              = 0x0200
	ssSunken                   = 0x1000
	cbsDropdownList            = 0x0003
	swShow                     = 5
	swRestore                  = 9
	wmDestroy                  = 0x0002
	wmSize                     = 0x0005
	wmGetMinMaxInfo            = 0x0024
	wmSetFont                  = 0x0030
	wmSetIcon                  = 0x0080
	wmSetText                  = 0x000C
	wmGetText                  = 0x000D
	wmGetTextLength            = 0x000E
	wmCommand                  = 0x0111
	wmSetSel                   = 0x00B1
	wmReplaceSel               = 0x00C2
	emSetLimitText             = 0x00C5
	emScrollCaret              = 0x00B7
	emSetCueBanner             = 0x1501
	cbAddString                = 0x0143
	cbGetCurSel                = 0x0147
	cbSetCurSel                = 0x014E
	wmTimer                    = 0x0113
	wmClose                    = 0x0010
	idcArrow                   = 32512
	colorWindow                = 5
	defaultGUIFont             = 17
	imageIcon                  = 1
	iconSmall                  = 0
	iconBig                    = 1
	lrShared                   = 0x00008000
	appIconResourceID          = 1
	timerID                    = 1
	statusHeight               = 170
	toolbarHeight              = 38
	toolbarControlH            = 24
	retentionComboH            = 150
	retentionControlID         = 1001
	searchEditID               = 1002
	searchButtonID             = 1003
	cbnSelChange               = 1
	bnClicked                  = 0
	hwndBottom                 = 1
	swpNoSize                  = 0x0001
	swpNoMove                  = 0x0002
	swpNoActivate              = 0x0010
	errClassExists             = 1410
	smCXScreen                 = 0
	smCYScreen                 = 1
	smCXIcon                   = 11
	smCYIcon                   = 12
	smCXSmallIcon              = 49
	smCYSmallIcon              = 50
	legacyDefaultWinW          = 720
	legacyDefaultWinH          = 560
	defaultWinW                = 800
	defaultWinH                = 680
	minimumWinW                = 420
	minimumWinH                = 360
	logTailBytes         int64 = 4 << 20
	maxLogIncrementBytes int64 = 1 << 20
	maxLogEditCharacters       = 7 << 20
)

type wndClassEx struct {
	Size       uint32
	Style      uint32
	WndProc    uintptr
	ClsExtra   int32
	WndExtra   int32
	Instance   uintptr
	Icon       uintptr
	Cursor     uintptr
	Background uintptr
	MenuName   *uint16
	ClassName  *uint16
	IconSm     uintptr
}

type point struct{ X, Y int32 }
type msg struct {
	Hwnd    uintptr
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      point
}
type rect struct{ Left, Top, Right, Bottom int32 }

type minMaxInfo struct {
	Reserved     point
	MaxSize      point
	MaxPosition  point
	MinTrackSize point
	MaxTrackSize point
}

type retentionChoice struct {
	days  int
	label string
}

var retentionChoices = []retentionChoice{
	{days: 0, label: "关闭"},
	{days: 3, label: "3 个使用日"},
	{days: 7, label: "7 个使用日"},
	{days: 14, label: "14 个使用日"},
	{days: 30, label: "30 个使用日"},
}

type winState struct {
	path           string
	status         StatusFunc
	getRetention   func() (int, error)
	setRetention   func(int) error
	editSt         uintptr
	toolbar        uintptr
	retentionLabel uintptr
	retentionCombo uintptr
	searchEdit     uintptr
	searchButton   uintptr
	editLg         uintptr
	offset         int64
	hwnd           uintptr
	retentionDays  int
	lastStatus     string // skip SetText when unchanged to preserve selection/copy
	searchQuery    string
	searchAfter    int
	searchActive   bool
	searchSelStart uintptr
	searchSelEnd   uintptr
}

func lastErr() uint32 {
	e, _, _ := pGetLastError.Call()
	return uint32(e) // #nosec G115 -- GetLastError is defined to return a DWORD.
}

func logf(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	p := appinfo.LogPath()
	f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(f, "%s [logui] %s\n", time.Now().Format("2006/01/02 15:04:05"), msg)
	_ = f.Close()
}

// Open shows status + live log. Closing the window does not stop the tray proxy.
func Open(
	path string,
	status StatusFunc,
	getRetention func() (int, error),
	setRetention func(int) error,
) error {
	_ = os.MkdirAll(filepath.Dir(path), 0o700)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	_ = f.Close()
	if status == nil {
		status = func() (string, string) { return "—", "" }
	}

	openMu.Lock()
	if openHWND != 0 {
		hwnd := openHWND
		openMu.Unlock()
		pShowWindow.Call(hwnd, swRestore)
		pSetForeground.Call(hwnd)
		logf("focus existing hwnd=%#x", hwnd)
		return nil
	}
	if building {
		openMu.Unlock()
		return nil // a window is already being created for this request
	}
	building = true
	openMu.Unlock()

	type readyMsg struct {
		err error
	}
	ready := make(chan readyMsg, 1)

	go func() {
		runtime.LockOSThread()
		hwnd, st, err := buildWindow(path, status, getRetention, setRetention)
		if err != nil {
			logf("buildWindow failed: %v", err)
			ready <- readyMsg{err: err}
			return
		}
		logf("buildWindow ok hwnd=%#x", hwnd)
		ready <- readyMsg{err: nil}
		pump(hwnd, st) // blocks until window closed
		logf("pump ended hwnd=%#x", hwnd)
	}()

	// buildWindow succeeded: openHWND is set inside buildWindow, so the build is
	// no longer in flight. If it failed or timed out, reset building so a later
	// click can retry.
	openMu.Lock()
	building = false
	openMu.Unlock()

	select {
	case r := <-ready:
		return r.err
	case <-time.After(5 * time.Second):
		return fmt.Errorf("创建状态与日志窗口超时，请查看 %s", path)
	}
}

func buildWindow(
	path string,
	status StatusFunc,
	getRetention func() (int, error),
	setRetention func(int) error,
) (uintptr, *winState, error) {
	hInstance, _, _ := pGetModuleHandleW.Call(0)
	largeIcon := loadWindowIcon(hInstance, smCXIcon, smCYIcon)
	smallIcon := loadWindowIcon(hInstance, smCXSmallIcon, smCYSmallIcon)

	classOnce.Do(func() {
		var err error
		className, err = syscall.UTF16PtrFromString("hellogrok.MonitorWindow.v6")
		if err != nil {
			classRegErr = err
			return
		}
		cursor, _, _ := pLoadCursorW.Call(0, idcArrow)
		wc := wndClassEx{
			Size:       uint32(unsafe.Sizeof(wndClassEx{})),
			WndProc:    windowProcCallback,
			Instance:   hInstance,
			Icon:       largeIcon,
			Cursor:     cursor,
			Background: uintptr(colorWindow + 1),
			ClassName:  className,
			IconSm:     smallIcon,
		}
		atom, _, callErr := pRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc)))
		if atom == 0 && lastErr() != errClassExists {
			classRegErr = fmt.Errorf("RegisterClassExW: %v last=%d", callErr, lastErr())
			return
		}
		logf("RegisterClassEx atom=%#x", atom)
	})
	if classRegErr != nil {
		return 0, nil, classRegErr
	}

	st := &winState{
		path: path, status: status,
		getRetention: getRetention, setRetention: setRetention,
		retentionDays: prefs.DefaultLogRetentionUsageDays,
	}
	title, _ := syscall.UTF16PtrFromString("hellogrok " + appinfo.Version + " — 状态与日志（关闭不影响代理）")
	x, y, w, h := loadGeometry()
	hwnd, _, callErr := pCreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(className)),
		uintptr(unsafe.Pointer(title)),
		wsOverlappedWindow,
		uintptr(x), uintptr(y), uintptr(w), uintptr(h),
		0, 0, hInstance, 0,
	)
	if hwnd == 0 {
		return 0, nil, fmt.Errorf("CreateWindowExW: %v last=%d", callErr, lastErr())
	}
	st.hwnd = hwnd
	pSendMessageW.Call(hwnd, wmSetIcon, iconBig, largeIcon)
	pSendMessageW.Call(hwnd, wmSetIcon, iconSmall, smallIcon)

	editClass, _ := syscall.UTF16PtrFromString("EDIT")
	mkEdit := func(wrap bool) (uintptr, error) {
		h, _, e := pCreateWindowExW.Call(
			0,
			uintptr(unsafe.Pointer(editClass)),
			0,
			editStyle(wrap),
			0, 0, 200, 80,
			hwnd, 0, hInstance, 0,
		)
		if h == 0 {
			return 0, fmt.Errorf("EDIT: %v last=%d", e, lastErr())
		}
		return h, nil
	}
	var err error
	if st.editSt, err = mkEdit(true); err != nil {
		pDestroyWindow.Call(hwnd)
		return 0, nil, err
	}
	if st.editLg, err = mkEdit(false); err != nil {
		pDestroyWindow.Call(hwnd)
		return 0, nil, err
	}
	staticClass, _ := syscall.UTF16PtrFromString("STATIC")
	comboClass, _ := syscall.UTF16PtrFromString("COMBOBOX")
	buttonClass, _ := syscall.UTF16PtrFromString("BUTTON")
	createChild := func(class *uint16, text string, style uintptr, id int, height int) (uintptr, error) {
		caption, _ := syscall.UTF16PtrFromString(text)
		h, _, e := pCreateWindowExW.Call(
			0, uintptr(unsafe.Pointer(class)), uintptr(unsafe.Pointer(caption)), style,
			0, 0, 100, uintptr(height), hwnd, uintptr(id), hInstance, 0,
		)
		if h == 0 {
			return 0, fmt.Errorf("create child control %d: %v last=%d", id, e, lastErr())
		}
		return h, nil
	}
	if st.toolbar, err = createChild(staticClass, "", wsChild|wsVisible|wsClipSiblings|ssSunken, 0, toolbarHeight); err != nil {
		pDestroyWindow.Call(hwnd)
		return 0, nil, err
	}
	if st.retentionLabel, err = createChild(staticClass, "自动清理天数", wsChild|wsVisible|ssCenterImage, 0, toolbarControlH); err != nil {
		pDestroyWindow.Call(hwnd)
		return 0, nil, err
	}
	if st.retentionCombo, err = createChild(comboClass, "", wsChild|wsVisible|wsVScroll|wsTabStop|cbsDropdownList, retentionControlID, retentionComboH); err != nil {
		pDestroyWindow.Call(hwnd)
		return 0, nil, err
	}
	if st.searchEdit, err = createChild(editClass, "", wsChild|wsVisible|wsBorder|wsTabStop|esAutohscroll, searchEditID, toolbarControlH); err != nil {
		pDestroyWindow.Call(hwnd)
		return 0, nil, err
	}
	if st.searchButton, err = createChild(buttonClass, "搜索", wsChild|wsVisible|wsTabStop, searchButtonID, toolbarControlH); err != nil {
		pDestroyWindow.Call(hwnd)
		return 0, nil, err
	}
	pSetWindowPos.Call(st.toolbar, hwndBottom, 0, 0, 0, 0, swpNoSize|swpNoMove|swpNoActivate)
	pSendMessageW.Call(st.editLg, emSetLimitText, 8<<20, 0)
	hfont, _, _ := pGetStockObject.Call(defaultGUIFont)
	for _, control := range []uintptr{
		st.editSt, st.toolbar, st.retentionLabel, st.retentionCombo,
		st.searchEdit, st.searchButton, st.editLg,
	} {
		pSendMessageW.Call(control, wmSetFont, hfont, 1)
	}
	searchCue, _ := syscall.UTF16PtrFromString("搜索日志")
	pSendMessageW.Call(st.searchEdit, emSetCueBanner, 1, uintptr(unsafe.Pointer(searchCue)))
	initializeRetention(st)

	refreshStatus(st)
	st.offset = reloadLog(st, logTailBytes)
	layout(st)

	keepAliveMu.Lock()
	keepAlive[hwnd] = st
	keepAliveMu.Unlock()
	openMu.Lock()
	openHWND = hwnd
	openMu.Unlock()

	pShowWindow.Call(hwnd, swShow)
	pUpdateWindow.Call(hwnd)
	pInvalidateRect.Call(st.retentionCombo, 0, 1)
	pUpdateWindow.Call(st.retentionCombo)
	pSetForeground.Call(hwnd)
	pSetTimer.Call(hwnd, timerID, 400, 0)
	return hwnd, st, nil
}

func loadWindowIcon(hInstance uintptr, widthMetric, heightMetric int) uintptr {
	width, _, _ := pGetSystemMetrics.Call(uintptr(widthMetric))
	height, _, _ := pGetSystemMetrics.Call(uintptr(heightMetric))
	icon, _, _ := pLoadImageW.Call(
		hInstance,
		appIconResourceID,
		imageIcon,
		width,
		height,
		lrShared,
	)
	return icon
}

func editStyle(wrap bool) uintptr {
	style := uintptr(wsChild | wsVisible | wsVScroll | wsBorder | wsClipSiblings |
		esMultiline | esReadonly | esAutovscroll | esWantreturn)
	if !wrap {
		style |= wsHScroll | esAutohscroll
	}
	return style
}

func pump(hwnd uintptr, st *winState) {
	var m msg
	for {
		r, _, _ := pGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(r) <= 0 { // #nosec G115 -- GetMessageW returns a signed 32-bit BOOL through syscall.Call.
			break
		}
		pTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		pDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
	}
	keepAliveMu.Lock()
	delete(keepAlive, hwnd)
	keepAliveMu.Unlock()
	openMu.Lock()
	if openHWND == hwnd {
		openHWND = 0
	}
	openMu.Unlock()
	_ = st
}

// fix Open goroutine to call pump after ready
// (Open already structured to call pump after ready <- nil)

func wndProc(hwnd uintptr, msg uint32, wParam, lParam uintptr) uintptr {
	st := stateFrom(hwnd)
	switch msg {
	case wmGetMinMaxInfo:
		if lParam != 0 {
			var info minMaxInfo
			size := unsafe.Sizeof(info)
			pRtlMoveMemory.Call(uintptr(unsafe.Pointer(&info)), lParam, size)
			info.MinTrackSize = point{X: minimumWinW, Y: minimumWinH}
			pRtlMoveMemory.Call(lParam, uintptr(unsafe.Pointer(&info)), size)
		}
		return 0
	case wmSize:
		if st != nil {
			layout(st)
		}
		return 0
	case wmTimer:
		if st != nil && wParam == timerID {
			refreshStatus(st)
			appendNew(st)
		}
		return 0
	case wmCommand:
		if st == nil {
			break
		}
		controlID := int(wParam & 0xffff)
		notification := int((wParam >> 16) & 0xffff)
		switch {
		case controlID == retentionControlID && notification == cbnSelChange:
			handleRetentionChange(st)
			return 0
		case controlID == searchButtonID && notification == bnClicked:
			handleSearch(st)
			return 0
		}
	case wmClose:
		// remember geometry before destroy
		if st != nil {
			saveGeometry(hwnd)
		}
		pDestroyWindow.Call(hwnd)
		return 0
	case wmDestroy:
		pKillTimer.Call(hwnd, timerID)
		pPostQuitMessage.Call(0)
		return 0
	}
	r, _, _ := pDefWindowProcW.Call(hwnd, uintptr(msg), wParam, lParam)
	return r
}

func stateFrom(hwnd uintptr) *winState {
	keepAliveMu.Lock()
	defer keepAliveMu.Unlock()
	return keepAlive[hwnd]
}

func layout(st *winState) {
	var rc rect
	ok, _, _ := pGetClientRect.Call(st.hwnd, uintptr(unsafe.Pointer(&rc)))
	width, height := rc.Right-rc.Left, rc.Bottom-rc.Top
	if ok == 0 || width <= 0 || height <= 0 {
		return
	}
	w := int(width)
	h := int(height)
	sh := statusPanelHeight(h)
	if st.editSt != 0 {
		pMoveWindow.Call(st.editSt, 0, 0, uintptr(w), uintptr(sh), 1)
	}
	if st.toolbar != 0 {
		pMoveWindow.Call(st.toolbar, 0, uintptr(sh), uintptr(w), toolbarHeight, 1)
	}

	const (
		margin        = 8
		labelW        = 88
		comboW        = 108
		buttonW       = 56
		controlGap    = 6
		sectionGap    = 12
		searchIdealW  = 180
		searchMinimum = 72
	)
	searchW := w - 2*margin - labelW - comboW - buttonW - 2*controlGap - sectionGap
	if searchW > searchIdealW {
		searchW = searchIdealW
	}
	if searchW < searchMinimum {
		searchW = searchMinimum
	}
	groupW := labelW + controlGap + comboW + sectionGap + searchW + controlGap + buttonW
	x := w - margin - groupW
	if x < margin {
		x = margin
	}
	y := sh + (toolbarHeight-toolbarControlH)/2
	move := func(control uintptr, left, width, height int) {
		if control != 0 {
			pMoveWindow.Call(control, uintptr(left), uintptr(y), uintptr(width), uintptr(height), 1)
		}
	}
	move(st.retentionLabel, x, labelW, toolbarControlH)
	x += labelW + controlGap
	move(st.retentionCombo, x, comboW, retentionComboH)
	x += comboW + sectionGap
	move(st.searchEdit, x, searchW, toolbarControlH)
	x += searchW + controlGap
	move(st.searchButton, x, buttonW, toolbarControlH)
	if st.retentionCombo != 0 {
		pInvalidateRect.Call(st.retentionCombo, 0, 1)
		pUpdateWindow.Call(st.retentionCombo)
	}

	logY := sh + toolbarHeight
	if logY > h {
		logY = h
	}
	if st.editLg != 0 {
		pMoveWindow.Call(st.editLg, 0, uintptr(logY), uintptr(w), uintptr(h-logY), 1)
	}
}

func refreshStatus(st *winState) {
	short, detail := "—", ""
	if st.status != nil {
		short, detail = st.status()
	}
	text := "【运行状态】 " + short
	if strings.TrimSpace(detail) != "" {
		text += "\r\n\r\n" + toCRLF(detail)
	}
	// WM_SETTEXT clears selection; only update when content actually changes.
	if text == st.lastStatus {
		return
	}
	st.lastStatus = text
	setEditText(st.editSt, text)
}

func statusPanelHeight(clientHeight int) int {
	height := statusHeight
	if clientHeight < height+toolbarHeight+80 {
		height = (clientHeight - toolbarHeight) / 3
		if height < 80 {
			height = 80
		}
	}
	return height
}

func reloadLog(st *winState, max int64) int64 {
	data, size, err := readTailFile(st.path, max)
	if err != nil {
		setEditText(st.editLg, "无法读取日志:\r\n"+st.path+"\r\n"+err.Error())
		return 0
	}
	setEditText(st.editLg, "【日志】 "+st.path+"\r\n"+
		"----------------------------------------\r\n"+
		toCRLF(string(data)))
	resetSearch(st)
	scrollToEnd(st.editLg)
	return size
}

func appendNew(st *winState) {
	fi, err := os.Stat(st.path)
	if err != nil {
		return
	}
	size := fi.Size()
	if size < st.offset {
		st.offset = reloadLog(st, logTailBytes)
		return
	}
	if size == st.offset {
		return
	}
	delta := size - st.offset
	currentLength, _, _ := pSendMessageW.Call(st.editLg, wmGetTextLength, 0, 0)
	if delta > maxLogIncrementBytes || currentLength >= maxLogEditCharacters ||
		int64(currentLength)+delta > maxLogEditCharacters {
		st.offset = reloadLog(st, logTailBytes)
		return
	}
	f, err := os.Open(st.path)
	if err != nil {
		return
	}
	defer f.Close()
	if _, err := f.Seek(st.offset, 0); err != nil {
		return
	}
	buf, err := io.ReadAll(io.LimitReader(f, delta))
	if err != nil || len(buf) == 0 {
		return
	}
	st.offset += int64(len(buf))
	appendEdit(st.editLg, toCRLF(string(buf)))
	if st.searchActive {
		pSendMessageW.Call(st.editLg, wmSetSel, st.searchSelStart, st.searchSelEnd)
		pSendMessageW.Call(st.editLg, emScrollCaret, 0, 0)
	} else {
		scrollToEnd(st.editLg)
	}
}

func initializeRetention(st *winState) {
	for _, choice := range retentionChoices {
		label, _ := syscall.UTF16PtrFromString(choice.label)
		pSendMessageW.Call(st.retentionCombo, cbAddString, 0, uintptr(unsafe.Pointer(label)))
	}
	if st.getRetention != nil {
		days, err := st.getRetention()
		if err != nil {
			logf("load log retention preference failed: %v", err)
		} else if prefs.ValidLogRetentionUsageDays(days) {
			st.retentionDays = days
		}
	}
	setRetentionSelection(st, st.retentionDays)
}

func setRetentionSelection(st *winState, days int) {
	for index, choice := range retentionChoices {
		if choice.days == days {
			pSendMessageW.Call(st.retentionCombo, cbSetCurSel, uintptr(index), 0)
			return
		}
	}
}

func handleRetentionChange(st *winState) {
	selected, _, _ := pSendMessageW.Call(st.retentionCombo, cbGetCurSel, 0, 0)
	index := int(int32(selected))
	if index < 0 || index >= len(retentionChoices) {
		return
	}
	days := retentionChoices[index].days
	if days == st.retentionDays {
		return
	}
	if st.setRetention == nil {
		setRetentionSelection(st, st.retentionDays)
		return
	}
	if err := st.setRetention(days); err != nil {
		setRetentionSelection(st, st.retentionDays)
		logf("save log retention preference failed: %v", err)
		dialog.Info("hellogrok", "保存日志自动清理天数失败：\n"+err.Error())
		return
	}
	st.retentionDays = days
	logf("log retention preference changed: keep_usage_days=%d (applies on next application start)", days)
}

func handleSearch(st *winState) {
	query := strings.TrimSpace(windowText(st.searchEdit))
	if query == "" {
		pSetFocus.Call(st.searchEdit)
		return
	}
	text := windowText(st.editLg)
	if query != st.searchQuery {
		st.searchQuery = query
		st.searchAfter = 0
	}
	start, end, ok := nextMatch(text, query, st.searchAfter)
	if !ok {
		resetSearch(st)
		dialog.Info("hellogrok 日志搜索", "未找到："+query)
		return
	}
	st.searchAfter = end
	st.searchActive = true
	st.searchSelStart = utf16Length(text[:start])
	st.searchSelEnd = utf16Length(text[:end])
	pSendMessageW.Call(st.editLg, wmSetSel, st.searchSelStart, st.searchSelEnd)
	pSendMessageW.Call(st.editLg, emScrollCaret, 0, 0)
	pSetFocus.Call(st.editLg)
}

func resetSearch(st *winState) {
	st.searchQuery = ""
	st.searchAfter = 0
	st.searchActive = false
	st.searchSelStart = 0
	st.searchSelEnd = 0
}

func windowText(hwnd uintptr) string {
	if hwnd == 0 {
		return ""
	}
	length, _, _ := pSendMessageW.Call(hwnd, wmGetTextLength, 0, 0)
	if length == 0 {
		return ""
	}
	buffer := make([]uint16, int(length)+1)
	pSendMessageW.Call(hwnd, wmGetText, uintptr(len(buffer)), uintptr(unsafe.Pointer(&buffer[0])))
	return syscall.UTF16ToString(buffer)
}

func utf16Length(value string) uintptr {
	length := 0
	for _, r := range value {
		length++
		if r > 0xffff {
			length++
		}
	}
	return uintptr(length)
}

func setEditText(edit uintptr, text string) {
	if edit == 0 {
		return
	}
	p, _ := syscall.UTF16PtrFromString(text)
	pSendMessageW.Call(edit, wmSetText, 0, uintptr(unsafe.Pointer(p)))
}

func appendEdit(edit uintptr, text string) {
	if edit == 0 {
		return
	}
	lenR, _, _ := pSendMessageW.Call(edit, wmGetTextLength, 0, 0)
	pSendMessageW.Call(edit, wmSetSel, lenR, lenR)
	p, _ := syscall.UTF16PtrFromString(text)
	pSendMessageW.Call(edit, wmReplaceSel, 0, uintptr(unsafe.Pointer(p)))
}

func scrollToEnd(edit uintptr) {
	if edit == 0 {
		return
	}
	lenR, _, _ := pSendMessageW.Call(edit, wmGetTextLength, 0, 0)
	pSendMessageW.Call(edit, wmSetSel, lenR, lenR)
	pSendMessageW.Call(edit, emScrollCaret, 0, 0)
}

func toCRLF(s string) string {
	out := make([]byte, 0, len(s)+64)
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\n' {
			if i == 0 || s[i-1] != '\r' {
				out = append(out, '\r', '\n')
			} else {
				out = append(out, '\n')
			}
			continue
		}
		out = append(out, c)
	}
	return string(out)
}

// geometryJSON is persisted under %LOCALAPPDATA%\hellogrok\window.json
type geometryJSON struct {
	X int `json:"x"`
	Y int `json:"y"`
	W int `json:"w"`
	H int `json:"h"`
}

func geometryPath() string {
	return filepath.Join(appinfo.DataDir(), "window.json")
}

func defaultGeometry() (x, y, w, h int) {
	w, h = defaultWinW, defaultWinH
	sw, _, _ := pGetSystemMetrics.Call(smCXScreen)
	sh, _, _ := pGetSystemMetrics.Call(smCYScreen)
	if sw == 0 {
		sw = 1920
	}
	if sh == 0 {
		sh = 1080
	}
	// horizontal center, slightly above vertical center
	x = (int(sw) - w) / 2
	y = (int(sh)-h)/2 - int(sh)/20
	if y < 20 {
		y = 20
	}
	if x < 0 {
		x = 0
	}
	return
}

func loadGeometry() (x, y, w, h int) {
	x, y, w, h = defaultGeometry()
	b, err := os.ReadFile(geometryPath())
	if err != nil {
		return
	}
	var g geometryJSON
	if json.Unmarshal(b, &g) != nil {
		return
	}
	g = migrateLegacyWindowSize(g)
	if g.W >= 400 && g.H >= 300 {
		w, h = g.W, g.H
	}
	// keep on-screen roughly
	sw, _, _ := pGetSystemMetrics.Call(smCXScreen)
	sh, _, _ := pGetSystemMetrics.Call(smCYScreen)
	if g.X > -100 && g.Y > -100 && int(sw) > 0 && g.X < int(sw)-50 && g.Y < int(sh)-50 {
		x, y = g.X, g.Y
	}
	return
}

func migrateLegacyWindowSize(g geometryJSON) geometryJSON {
	if g.W == legacyDefaultWinW && g.H == legacyDefaultWinH {
		g.W, g.H = defaultWinW, defaultWinH
	}
	return g
}

func saveGeometry(hwnd uintptr) {
	var rc rect
	r, _, _ := pGetWindowRect.Call(hwnd, uintptr(unsafe.Pointer(&rc)))
	if r == 0 {
		return
	}
	g := geometryJSON{
		X: int(rc.Left),
		Y: int(rc.Top),
		W: int(rc.Right - rc.Left),
		H: int(rc.Bottom - rc.Top),
	}
	if g.W < 200 || g.H < 150 {
		return
	}
	_ = os.MkdirAll(filepath.Dir(geometryPath()), 0o700)
	b, err := json.MarshalIndent(g, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(geometryPath(), b, 0o600)
	logf("saved geometry %+v", g)
}
