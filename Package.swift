// swift-tools-version: 5.9
import PackageDescription

let package = Package(
    name: "Clearline",
    platforms: [.macOS(.v13)],
    products: [.executable(name: "Clearline", targets: ["Clearline"]), .executable(name: "ClearlineNetworkAgent", targets: ["ClearlineNetworkAgent"])],
    targets: [
        .target(name: "ClearlineKit", linkerSettings: [.linkedFramework("Security"), .linkedFramework("SystemConfiguration")]),
        .executableTarget(name: "Clearline", dependencies: ["ClearlineKit"], linkerSettings: [.linkedFramework("SwiftUI"), .linkedFramework("AppKit"), .linkedFramework("Network")]),
        .executableTarget(name: "ClearlineNetworkAgent", dependencies: ["ClearlineKit"]),
        .testTarget(name: "ClearlineKitTests", dependencies: ["ClearlineKit"]),
        .testTarget(name: "ClearlineAppTests", dependencies: ["Clearline"])
    ], swiftLanguageVersions: [.v5]
)
