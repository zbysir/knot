// Knot.app -- the native shell.
//
// The window is AppKit, the content is the panel the Go helper serves on
// loopback. Not a browser: the helper is a child process this app owns and
// ends, the window is a real window with a real menu bar, and nothing about it
// is reachable from outside this machine.
//
// The alternative -- rewriting the panel in SwiftUI -- would mean two copies of
// every screen and a second place for them to disagree. This way the panel has
// one implementation and the shell stays small enough to read in one sitting.

import AppKit
import WebKit

// extraArgs are whatever the app was launched with, forwarded to the helper:
//
//     open -a Knot --args --data ~/.knot-staging --singbox /path/to/sing-box
//
// Useful for a second profile, or for pointing a trial run at something other
// than the real one. The process serial number macOS passes on some launches is
// not an argument anybody meant.
let extraArgs: [String] = CommandLine.arguments.dropFirst().filter { !$0.hasPrefix("-psn_") }

// panelURL follows --ui when it is given, because otherwise the window would
// wait on a port the helper was told not to use.
let panelURL: URL = {
    if let i = extraArgs.firstIndex(of: "--ui"), i + 1 < extraArgs.count {
        if let u = URL(string: "http://" + extraArgs[i + 1]) { return u }
    }
    return URL(string: "http://127.0.0.1:8765")!
}()

final class AppDelegate: NSObject, NSApplicationDelegate, WKNavigationDelegate, WKUIDelegate {
    private var window: NSWindow!
    private var web: WKWebView!
    private var helper: Process?
    private var helperLog = ""
    private var attempts = 0
    private var quitting = false

    // MARK: - lifecycle

    func applicationDidFinishLaunching(_ note: Notification) {
        buildMenu()
        buildWindow()
        startHelper()
        load()
    }

    func applicationWillTerminate(_ note: Notification) {
        quitting = true
        stopHelper()
    }

    // Closing the window must not drop the tunnels: an operator in the middle
    // of a database session should not lose it to a stray cmd-W. The app keeps
    // running and the Dock icon brings the window back; cmd-Q is how you leave.
    func applicationShouldTerminateAfterLastWindowClosed(_ app: NSApplication) -> Bool { false }

    func applicationShouldHandleReopen(_ app: NSApplication, hasVisibleWindows flag: Bool) -> Bool {
        if !flag { window.makeKeyAndOrderFront(nil) }
        return true
    }

    // MARK: - the helper

    private var helperPath: URL {
        Bundle.main.bundleURL.appendingPathComponent("Contents/MacOS/knot-helper")
    }

    private func startHelper() {
        let p = Process()
        p.executableURL = helperPath
        // --no-open, because the window below IS the panel. Without it the
        // helper would also throw the page at Safari.
        // --exit-with-parent so a crash of this shell does not leave the
        // helper holding the panel port and every forward with no window to
        // close it from.
        p.arguments = ["connect", "--no-open", "--exit-with-parent"] + extraArgs

        let pipe = Pipe()
        p.standardError = pipe
        p.standardOutput = pipe
        pipe.fileHandleForReading.readabilityHandler = { [weak self] h in
            guard let s = String(data: h.availableData, encoding: .utf8), !s.isEmpty else { return }
            DispatchQueue.main.async {
                self?.helperLog += s
                // Keep the tail only; this runs for days.
                if let t = self?.helperLog, t.count > 8000 {
                    self?.helperLog = String(t.suffix(4000))
                }
            }
        }

        p.terminationHandler = { [weak self] _ in
            DispatchQueue.main.async { self?.helperDied() }
        }
        do {
            try p.run()
            helper = p
        } catch {
            fail("启动失败", "找不到或无法运行 knot-helper。\n\n\(error.localizedDescription)")
        }
    }

    private func stopHelper() {
        guard let p = helper, p.isRunning else { return }
        p.terminationHandler = nil
        p.terminate()
        // Give it a moment to close its listeners and its relay sessions.
        let deadline = Date().addingTimeInterval(3)
        while p.isRunning && Date() < deadline {
            usleep(50_000)
        }
        if p.isRunning { kill(p.processIdentifier, SIGKILL) }
    }

    // helperDied decides whether the app is over.
    //
    // A helper that exits immediately is usually not a failure: it found
    // another knot already holding the panel port and handed the window over,
    // which is exactly what should happen when the app is opened twice. So ask
    // the panel before concluding anything.
    private func helperDied() {
        guard !quitting else { return }
        helper = nil
        panelIsUp { up in
            if up { return } // somebody else is serving it; carry on
            self.fail("knot 已退出", self.helperLog.isEmpty
                ? "后台进程结束了。"
                : "后台进程结束了：\n\n\(self.helperLog.suffix(1200))")
        }
    }

    private func panelIsUp(_ done: @escaping (Bool) -> Void) {
        var req = URLRequest(url: panelURL.appendingPathComponent("api/state"))
        req.timeoutInterval = 2
        URLSession.shared.dataTask(with: req) { _, resp, _ in
            let ok = (resp as? HTTPURLResponse)?.statusCode == 200
            DispatchQueue.main.async { done(ok) }
        }.resume()
    }

    // MARK: - the window

    private func buildWindow() {
        let cfg = WKWebViewConfiguration()
        web = WKWebView(frame: .zero, configuration: cfg)
        web.navigationDelegate = self
        web.uiDelegate = self
        // The panel is its own page; no browser chrome, no rubber-banding past
        // its edges.
        if web.responds(to: Selector(("setAllowsMagnification:"))) {
            web.allowsMagnification = false
        }
        web.setValue(false, forKey: "drawsBackground")

        window = NSWindow(
            contentRect: NSRect(x: 0, y: 0, width: 1000, height: 740),
            styleMask: [.titled, .closable, .miniaturizable, .resizable],
            backing: .buffered, defer: false)
        window.title = "Knot"
        window.contentView = web
        window.minSize = NSSize(width: 560, height: 420)
        // The panel is dark. Matching the window means no white flash while the
        // first paint is on its way.
        window.backgroundColor = NSColor(srgbRed: 0.059, green: 0.067, blue: 0.082, alpha: 1)
        window.setFrameAutosaveName("KnotMain")
        window.center()
        window.makeKeyAndOrderFront(nil)
        NSApp.activate(ignoringOtherApps: true)
    }

    private func load() {
        web.load(URLRequest(url: panelURL, cachePolicy: .reloadIgnoringLocalCacheData))
    }

    // The helper needs a moment to bind its port, so a first load that fails is
    // expected rather than exceptional. Retry quietly for a while before
    // admitting something is wrong.
    func webView(_ w: WKWebView, didFailProvisionalNavigation nav: WKNavigation!, withError error: Error) {
        attempts += 1
        if attempts > 60 {
            fail("连接不上面板", helperLog.isEmpty
                ? "后台进程没有在 \(panelURL.absoluteString) 上应答。"
                : "后台进程没有应答：\n\n\(helperLog.suffix(1200))")
            return
        }
        DispatchQueue.main.asyncAfter(deadline: .now() + 0.25) { [weak self] in self?.load() }
    }

    func webView(_ w: WKWebView, didFinish nav: WKNavigation!) { attempts = 0 }

    // Anything that is not our own panel goes to the real browser. There are no
    // such links today; this is what keeps that true if one ever appears.
    func webView(_ w: WKWebView, decidePolicyFor action: WKNavigationAction,
                 decisionHandler: @escaping (WKNavigationActionPolicy) -> Void) {
        guard let u = action.request.url else { return decisionHandler(.cancel) }
        if u.host == panelURL.host && u.port == panelURL.port {
            return decisionHandler(.allow)
        }
        NSWorkspace.shared.open(u)
        decisionHandler(.cancel)
    }

    // The panel asks for confirmation before deleting a forward. WKWebView
    // drops those on the floor unless the host answers them.
    func webView(_ w: WKWebView, runJavaScriptConfirmPanelWithMessage msg: String,
                 initiatedByFrame frame: WKFrameInfo,
                 completionHandler: @escaping (Bool) -> Void) {
        let a = NSAlert()
        a.messageText = msg
        a.addButton(withTitle: "确定")
        a.addButton(withTitle: "取消")
        completionHandler(a.runModal() == .alertFirstButtonReturn)
    }

    func webView(_ w: WKWebView, runJavaScriptAlertPanelWithMessage msg: String,
                 initiatedByFrame frame: WKFrameInfo,
                 completionHandler: @escaping () -> Void) {
        let a = NSAlert()
        a.messageText = msg
        a.runModal()
        completionHandler()
    }

    private func fail(_ title: String, _ body: String) {
        let a = NSAlert()
        a.alertStyle = .critical
        a.messageText = title
        a.informativeText = body
        a.addButton(withTitle: "退出")
        a.runModal()
        NSApp.terminate(nil)
    }

    // MARK: - menu

    @objc private func reload() { attempts = 0; load() }

    @objc private func openDataDir() {
        let dir = FileManager.default.homeDirectoryForCurrentUser.appendingPathComponent(".knot")
        NSWorkspace.shared.open(dir)
    }

    private func buildMenu() {
        let bar = NSMenu()

        let appItem = NSMenuItem()
        bar.addItem(appItem)
        let appMenu = NSMenu()
        appMenu.addItem(withTitle: "关于 Knot", action: #selector(NSApplication.orderFrontStandardAboutPanel(_:)), keyEquivalent: "")
        appMenu.addItem(.separator())
        appMenu.addItem(withTitle: "打开数据目录", action: #selector(openDataDir), keyEquivalent: "")
        appMenu.addItem(.separator())
        appMenu.addItem(withTitle: "隐藏 Knot", action: #selector(NSApplication.hide(_:)), keyEquivalent: "h")
        appMenu.addItem(.separator())
        appMenu.addItem(withTitle: "退出 Knot", action: #selector(NSApplication.terminate(_:)), keyEquivalent: "q")
        appItem.submenu = appMenu

        // Without an Edit menu the standard shortcuts do not reach the web
        // view's text fields, and pasting a join token -- the first thing
        // anybody does here -- silently does nothing.
        let editItem = NSMenuItem()
        bar.addItem(editItem)
        let edit = NSMenu(title: "编辑")
        edit.addItem(withTitle: "撤销", action: Selector(("undo:")), keyEquivalent: "z")
        edit.addItem(withTitle: "重做", action: Selector(("redo:")), keyEquivalent: "Z")
        edit.addItem(.separator())
        edit.addItem(withTitle: "剪切", action: #selector(NSText.cut(_:)), keyEquivalent: "x")
        edit.addItem(withTitle: "拷贝", action: #selector(NSText.copy(_:)), keyEquivalent: "c")
        edit.addItem(withTitle: "粘贴", action: #selector(NSText.paste(_:)), keyEquivalent: "v")
        edit.addItem(withTitle: "全选", action: #selector(NSText.selectAll(_:)), keyEquivalent: "a")
        editItem.submenu = edit

        let viewItem = NSMenuItem()
        bar.addItem(viewItem)
        let view = NSMenu(title: "显示")
        view.addItem(withTitle: "刷新", action: #selector(reload), keyEquivalent: "r")
        viewItem.submenu = view

        let winItem = NSMenuItem()
        bar.addItem(winItem)
        let win = NSMenu(title: "窗口")
        win.addItem(withTitle: "最小化", action: #selector(NSWindow.miniaturize(_:)), keyEquivalent: "m")
        win.addItem(withTitle: "缩放", action: #selector(NSWindow.zoom(_:)), keyEquivalent: "")
        winItem.submenu = win

        NSApp.mainMenu = bar
        NSApp.windowsMenu = win
    }
}

let app = NSApplication.shared
let delegate = AppDelegate()
app.delegate = delegate
app.setActivationPolicy(.regular)
app.run()
