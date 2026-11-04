import XCTest
@testable import ClearlineKit

final class ProxyRestoreTests: XCTestCase {
    func testRestorePreviousProxyAndRemovePreviouslyAbsentKeys() {
        let old: [String: Any] = ["HTTPEnable": 0, "ExceptionsList": ["internal.example"]]
        let installed: [String: Any] = ["HTTPEnable": 1, "HTTPProxy": "127.0.0.1", "HTTPPort": 12345]
        let current = old.merging(installed) { _, new in new }
        let restored = ProxySnapshot.restoring(current: current, before: old, installed: installed)
        XCTAssertEqual(restored["HTTPEnable"] as? Int, 0)
        XCTAssertNil(restored["HTTPProxy"])
        XCTAssertEqual(restored["ExceptionsList"] as? [String], ["internal.example"])
    }
    func testPreservesProxyChangesMadeByAnotherApp() {
        let old: [String: Any] = ["HTTPProxy": "old.proxy", "HTTPPort": 8080]
        let installed: [String: Any] = ["HTTPProxy": "127.0.0.1", "HTTPPort": 12345]
        let current: [String: Any] = ["HTTPProxy": "other.proxy", "HTTPPort": 9090, "HTTPSProxy": "unrelated"]
        let restored = ProxySnapshot.restoring(current: current, before: old, installed: installed)
        XCTAssertEqual(restored["HTTPProxy"] as? String, "other.proxy")
        XCTAssertEqual(restored["HTTPPort"] as? Int, 9090)
        XCTAssertEqual(restored["HTTPSProxy"] as? String, "unrelated")
    }
    func testHostOnlyExternalChangePreservesWholeEndpoint() {
        let old: [String: Any] = ["HTTPEnable": 0, "HTTPProxy": "old.proxy", "HTTPPort": 8080]
        let installed: [String: Any] = ["HTTPEnable": 1, "HTTPProxy": "127.0.0.1", "HTTPPort": 12345]
        let current: [String: Any] = ["HTTPEnable": 1, "HTTPProxy": "new.proxy", "HTTPPort": 12345]
        let restored = ProxySnapshot.restoring(current: current, before: old, installed: installed)
        XCTAssertEqual(restored["HTTPProxy"] as? String, "new.proxy")
        XCTAssertEqual(restored["HTTPPort"] as? Int, 12345)
        XCTAssertEqual(restored["HTTPEnable"] as? Int, 1)
    }
}
