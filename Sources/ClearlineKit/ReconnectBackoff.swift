public struct ReconnectBackoff {
    public private(set) var attempts = 0
    public init() {}
    public mutating func reset() { attempts = 0 }
    public mutating func nextDelay() -> Int {
        let delay = min(30, 1 << min(attempts, 5))
        // Saturate the counter as well as the delay for long running sessions.
        attempts = min(attempts + 1, 30)
        return delay
    }
}
