import XCTest
@testable import ClearlineKit

final class ReconnectBackoffTests: XCTestCase {
    func testRetriesContinueBeyondFiveWithBoundedRate() {
        var backoff = ReconnectBackoff()
        let delays = (0..<1000).map { _ in backoff.nextDelay() }
        XCTAssertEqual(Array(delays.prefix(6)), [1, 2, 4, 8, 16, 30])
        XCTAssertTrue(delays.dropFirst(6).allSatisfy { $0 == 30 })
        XCTAssertLessThanOrEqual(backoff.attempts, 30)
    }
    func testSuccessOrManualConnectRestartsShortRecoveryDelay() {
        var backoff = ReconnectBackoff()
        for _ in 0..<40 { _ = backoff.nextDelay() }
        backoff.reset()
        XCTAssertEqual(backoff.attempts, 0)
        XCTAssertEqual(backoff.nextDelay(), 1)
    }
}
