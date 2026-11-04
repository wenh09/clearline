import Foundation

/// Delivers all stdout events before the exit callback, including very short
/// lived children. Callbacks are always queued on the main thread in order.
public final class JSONChild {
    public let process = Process()
    private let input = Pipe()
    private let output = Pipe()
    private let errors = Pipe()
    private let delivery = DispatchQueue(label: "org.clearline.client.child-events")
    private let inputLock = NSLock()
    private var inputClosed = false
    private var buffer = Data()
    private var outputEnded = false
    private var exitStatus: Int32?
    private var exitDelivered = false
    public var onEvent: (@MainActor ([String: Any]) -> Void)?
    public var onExit: (@MainActor (Int32) -> Void)?

    public init(executable: URL, arguments: [String]) {
        process.executableURL = executable; process.arguments = arguments
        process.standardInput = input; process.standardOutput = output; process.standardError = errors
        errors.fileHandleForReading.readabilityHandler = { h in
            if h.availableData.isEmpty { h.readabilityHandler = nil }
        } // Never display upstream logs, keys or destinations.
        output.fileHandleForReading.readabilityHandler = { [weak self] h in
            let data = h.availableData
            if data.isEmpty { h.readabilityHandler = nil }
            self?.delivery.async { [weak self] in self?.accept(data) }
        }
        process.terminationHandler = { [weak self] p in
            self?.delivery.async { [weak self] in
                self?.exitStatus = p.terminationStatus
                self?.deliverExitIfReady()
            }
        }
    }
    private func accept(_ data: Data) {
        if data.isEmpty {
            outputEnded = true
            if !buffer.isEmpty { deliverLine(buffer); buffer.removeAll() }
            deliverExitIfReady()
            return
        }
        buffer.append(data)
        while let end = buffer.firstIndex(of: 10) {
            let line = Data(buffer[..<end]); buffer.removeSubrange(...end)
            deliverLine(line)
        }
        if buffer.count > 3_000_000 { buffer.removeAll() }
    }
    private func deliverLine(_ line: Data) {
        guard let event = try? JSONSerialization.jsonObject(with: line) as? [String: Any] else { return }
        DispatchQueue.main.async { [weak self] in self?.onEvent?(event) }
    }
    private func deliverExitIfReady() {
        guard outputEnded, let code = exitStatus, !exitDelivered else { return }
        exitDelivered = true
        DispatchQueue.main.async { [weak self] in self?.onExit?(code) }
    }
    public func start() throws { try process.run() }
    public func send(_ request: [String: Any]) throws {
        var data = try JSONSerialization.data(withJSONObject: request); data.append(10)
        inputLock.lock(); defer { inputLock.unlock() }
        guard !inputClosed else { throw CocoaError(.fileWriteNoPermission) }
        try input.fileHandleForWriting.write(contentsOf: data)
    }
    public func closeInput() {
        inputLock.lock(); defer { inputLock.unlock() }
        guard !inputClosed else { return }; inputClosed = true
        try? input.fileHandleForWriting.close()
    }
    public func stop() {
        closeInput()
        if process.isRunning { process.terminate() }
    }
    deinit {
        output.fileHandleForReading.readabilityHandler = nil; errors.fileHandleForReading.readabilityHandler = nil
        closeInput()
        if process.isRunning { process.terminate() }
    }
}
