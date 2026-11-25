// Hush menu bar app: shows the user service's state and threads that need
// attention, and opens threads in a terminal. It reads state through the
// bundled CLI (`hush service status --json`, `hush thread list --json`) when
// the menu opens and sends nothing while closed.
import AppKit
import ServiceManagement

struct Attention: Decodable {
    let title: String
    let status: String
    let threadId: String?
}

struct Sessions: Decodable {
    let open: Int
    let running: Int
}

struct ServiceStatus: Decodable {
    let state: String
    let sessions: Sessions?
    let attention: [Attention]
    let moreAttention: Int
    let startedAtMs: Int64?
    let log: String?
}

struct ThreadRow: Decodable {
    let id: String
    let title: String
    let status: String
}

struct ThreadPage: Decodable {
    let threads: [ThreadRow]
}

final class HushMenu: NSObject, NSApplicationDelegate, NSMenuDelegate {
    private let item = NSStatusBar.system.statusItem(withLength: NSStatusItem.squareLength)
    private let menu = NSMenu()
    private var status: ServiceStatus?
    private var active: [ThreadRow] = []
    private var loading = false

    private var cli: URL { Bundle.main.bundleURL.appendingPathComponent("Contents/MacOS/hush") }

    func applicationDidFinishLaunching(_ notification: Notification) {
        if let image = Bundle.main.image(forResource: "MenuTemplate") {
            image.isTemplate = true
            image.size = NSSize(width: 18, height: 18)
            item.button?.image = image
        } else {
            item.button?.title = "Hush"
        }
        item.button?.toolTip = "Hush"
        menu.delegate = self
        menu.autoenablesItems = false
        item.menu = menu
        rebuild()
    }

    func menuWillOpen(_ menu: NSMenu) { refresh() }

    // Runs the bundled CLI off the main thread and returns its stdout.
    private func run(_ args: [String]) -> Data? {
        let process = Process()
        process.executableURL = cli
        process.arguments = args
        let out = Pipe()
        process.standardOutput = out
        process.standardError = FileHandle.nullDevice
        do { try process.run() } catch { return nil }
        let data = out.fileHandleForReading.readDataToEndOfFile()
        process.waitUntilExit()
        return process.terminationStatus == 0 ? data : nil
    }

    private func refresh() {
        guard !loading else { return }
        loading = true
        DispatchQueue.global(qos: .userInitiated).async {
            let status = self.run(["service", "status", "--json"]).flatMap { try? JSONDecoder().decode(ServiceStatus.self, from: $0) }
            var active: [ThreadRow] = []
            if status?.state == "running", (status?.sessions?.running ?? 0) > 0,
               let data = self.run(["thread", "list", "--limit", "50", "--json"]),
               let page = try? JSONDecoder().decode(ThreadPage.self, from: data) {
                active = page.threads.filter { $0.status == "running" }
            }
            DispatchQueue.main.async {
                self.status = status
                self.active = active
                self.loading = false
                self.rebuild()
            }
        }
    }

    private func header(_ title: String, _ detail: String, color: NSColor) -> NSMenuItem {
        let text = NSMutableAttributedString(string: title + "\n", attributes: [.font: NSFont.boldSystemFont(ofSize: 13)])
        text.append(NSAttributedString(string: "● ", attributes: [.foregroundColor: color, .font: NSFont.systemFont(ofSize: 12)]))
        text.append(NSAttributedString(string: detail, attributes: [.foregroundColor: NSColor.secondaryLabelColor, .font: NSFont.systemFont(ofSize: 12)]))
        let row = NSMenuItem()
        row.attributedTitle = text
        row.isEnabled = false
        return row
    }

    private func section(_ title: String) -> NSMenuItem {
        let row = NSMenuItem()
        row.attributedTitle = NSAttributedString(string: title, attributes: [.font: NSFont.systemFont(ofSize: 11, weight: .semibold), .foregroundColor: NSColor.secondaryLabelColor])
        row.isEnabled = false
        return row
    }

    private func action(_ title: String, _ selector: Selector, key: String = "", represented: Any? = nil) -> NSMenuItem {
        let row = NSMenuItem(title: title, action: selector, keyEquivalent: key)
        row.target = self
        row.representedObject = represented
        return row
    }

    private func rebuild() {
        menu.removeAllItems()
        let s = status
        switch s?.state {
        case "running":
            let n = s?.attention.count ?? 0, more = s?.moreAttention ?? 0
            let attention = n + more > 0 ? " · \(n + more) need attention" : ""
            menu.addItem(header("Hush", "Running · \(s?.sessions?.running ?? 0) active\(attention)",
                                color: n + more > 0 ? .systemOrange : .systemGreen))
        case "paused": menu.addItem(header("Hush", "Paused", color: .systemYellow))
        case "unavailable": menu.addItem(header("Hush", "Not responding", color: .systemRed))
        case "not_installed": menu.addItem(header("Hush", "Service not installed", color: .systemGray))
        default: menu.addItem(header("Hush", loading ? "Checking…" : "Status unknown", color: .systemGray))
        }
        if let started = s?.startedAtMs, s?.state == "running" {
            let date = Date(timeIntervalSince1970: TimeInterval(started) / 1000)
            let row = NSMenuItem(title: "Backend started " + RelativeDateTimeFormatter().localizedString(for: date, relativeTo: Date()), action: nil, keyEquivalent: "")
            row.isEnabled = false
            menu.addItem(row)
        }
        let attention = (s?.attention ?? []).filter { $0.threadId != nil }
        if !attention.isEmpty {
            menu.addItem(.separator())
            menu.addItem(section("Needs attention"))
            for a in attention { menu.addItem(action("\(a.title) — \(a.status)", #selector(openThread(_:)), represented: a.threadId)) }
        }
        if !active.isEmpty {
            menu.addItem(.separator())
            menu.addItem(section("Running"))
            for t in active.prefix(8) { menu.addItem(action(t.title.isEmpty ? "Thread \(t.id)" : t.title, #selector(openThread(_:)), represented: t.id)) }
        }
        menu.addItem(.separator())
        menu.addItem(action("Open Hush", #selector(openHush), key: "o"))
        switch s?.state {
        case "running", "unavailable": menu.addItem(action("Restart Backend", #selector(restart)))
        case "paused": menu.addItem(action("Resume Backend", #selector(resume)))
        default: break
        }
        if s?.log != nil { menu.addItem(action("Show Service Log", #selector(showLog))) }
        menu.addItem(.separator())
        let openIn = NSMenuItem(title: "Open In", action: nil, keyEquivalent: "")
        let choices = NSMenu()
        for t in Terminal.allCases where available(t) {
            let row = action(t.title, #selector(chooseTerminal(_:)), represented: t.rawValue)
            row.state = t == terminal ? .on : .off
            choices.addItem(row)
        }
        openIn.submenu = choices
        menu.addItem(openIn)
        let login = action("Open Menu at Login", #selector(toggleLogin))
        login.state = SMAppService.mainApp.status == .enabled ? .on : .off
        menu.addItem(login)
        menu.addItem(action("Quit Hush Menu", #selector(quit), key: "q"))
    }

    // Terminals Hush can open in. Rex is driven through its CLI; the others
    // open a .command file, which needs no automation permission.
    private enum Terminal: String, CaseIterable {
        case rex, terminal, iterm, ghostty, system
        var title: String {
            switch self {
            case .rex: return "Rex"
            case .terminal: return "Terminal"
            case .iterm: return "iTerm"
            case .ghostty: return "Ghostty"
            case .system: return "Default App for .command Files"
            }
        }
        var bundleID: String? {
            switch self {
            case .rex: return "com.superlogical.rex"
            case .terminal: return "com.apple.Terminal"
            case .iterm: return "com.googlecode.iterm2"
            case .ghostty: return "com.mitchellh.ghostty"
            case .system: return nil
            }
        }
    }

    // The Rex CLI, which isn't on the PATH of an app started by Finder.
    private var rexCLI: URL? {
        let home = FileManager.default.homeDirectoryForCurrentUser.path
        return [home + "/Library/Application Support/com.superlogical.rex/RexServer/Versions/Current/Rex Server.app/Contents/MacOS/rex",
                home + "/.local/bin/rex", "/opt/homebrew/bin/rex", "/usr/local/bin/rex"]
            .first { FileManager.default.isExecutableFile(atPath: $0) }.map { URL(fileURLWithPath: $0) }
    }

    private func appURL(_ terminal: Terminal) -> URL? {
        terminal.bundleID.flatMap { NSWorkspace.shared.urlForApplication(withBundleIdentifier: $0) }
    }

    private func available(_ terminal: Terminal) -> Bool {
        switch terminal {
        case .system: return true
        case .rex: return rexCLI != nil && appURL(.rex) != nil
        default: return appURL(terminal) != nil
        }
    }

    private var terminal: Terminal {
        if let saved = UserDefaults.standard.string(forKey: "terminal").flatMap(Terminal.init(rawValue:)), available(saved) { return saved }
        return available(.rex) ? .rex : .terminal
    }

    private func openTerminal(_ args: [String]) {
        let command = [cli.path] + args
        if terminal == .rex, let rex = rexCLI {
            DispatchQueue.global(qos: .userInitiated).async {
                let process = Process()
                process.executableURL = rex
                process.arguments = ["run", "--label", "Hush", "--cwd", FileManager.default.homeDirectoryForCurrentUser.path, "--shell=none", "--"] + command
                process.standardOutput = FileHandle.nullDevice
                process.standardError = FileHandle.nullDevice
                let opened = (try? process.run()) != nil
                if opened { process.waitUntilExit() }
                DispatchQueue.main.async {
                    if opened && process.terminationStatus == 0, let app = self.appURL(.rex) {
                        NSWorkspace.shared.openApplication(at: app, configuration: NSWorkspace.OpenConfiguration())
                    } else {
                        self.openCommandFile(command, in: self.appURL(.terminal))
                    }
                }
            }
            return
        }
        if terminal == .ghostty, let app = appURL(.ghostty) {
            let config = NSWorkspace.OpenConfiguration()
            config.createsNewApplicationInstance = true
            config.arguments = ["-e"] + command
            NSWorkspace.shared.openApplication(at: app, configuration: config)
            return
        }
        openCommandFile(command, in: terminal == .system ? nil : appURL(terminal))
    }

    private func openCommandFile(_ command: [String], in app: URL?) {
        let quoted = command.map { "'" + $0.replacingOccurrences(of: "'", with: "'\\''") + "'" }.joined(separator: " ")
        let dir = FileManager.default.temporaryDirectory.appendingPathComponent("hush-menu", isDirectory: true)
        try? FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
        let script = dir.appendingPathComponent("open-\(UUID().uuidString).command")
        let body = "#!/bin/sh\nrm -f \"$0\"\ncd \"$HOME\"\nexec \(quoted)\n"
        guard FileManager.default.createFile(atPath: script.path, contents: Data(body.utf8), attributes: [.posixPermissions: 0o700]) else { return }
        if let app {
            NSWorkspace.shared.open([script], withApplicationAt: app, configuration: NSWorkspace.OpenConfiguration())
        } else {
            NSWorkspace.shared.open(script)
        }
    }

    @objc private func chooseTerminal(_ sender: NSMenuItem) {
        if let raw = sender.representedObject as? String { UserDefaults.standard.set(raw, forKey: "terminal") }
        rebuild()
    }

    @objc private func openHush() { openTerminal([]) }
    @objc private func openThread(_ sender: NSMenuItem) {
        if let id = sender.representedObject as? String { openTerminal(["--thread", id]) }
    }
    @objc private func restart() { background(["service", "restart"]) }
    @objc private func resume() { background(["service", "resume"]) }
    @objc private func showLog() {
        if let log = status?.log { NSWorkspace.shared.open(URL(fileURLWithPath: log)) }
    }
    @objc private func toggleLogin() {
        do {
            if SMAppService.mainApp.status == .enabled { try SMAppService.mainApp.unregister() } else { try SMAppService.mainApp.register() }
        } catch {
            NSAlert(error: error).runModal()
        }
        rebuild()
    }
    @objc private func quit() { NSApp.terminate(nil) }

    private func background(_ args: [String]) {
        DispatchQueue.global(qos: .userInitiated).async { _ = self.run(args) }
    }
}

let app = NSApplication.shared
let delegate = HushMenu()
app.delegate = delegate
app.setActivationPolicy(.accessory)
app.run()
