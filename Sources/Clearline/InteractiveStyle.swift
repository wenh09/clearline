import AppKit
import SwiftUI

/// AppKit owns cursor enter/exit handling, so view replacement or a disabled
/// control cannot leave an unbalanced NSCursor push/pop stack behind.
private final class CursorRegionView: NSView {
    var enabled = true
    override func viewDidMoveToWindow() {
        super.viewDidMoveToWindow(); window?.invalidateCursorRects(for: self)
    }
    override func setFrameSize(_ newSize: NSSize) {
        super.setFrameSize(newSize); window?.invalidateCursorRects(for: self)
    }
    override func hitTest(_ point: NSPoint) -> NSView? { nil }
    override func resetCursorRects() {
        super.resetCursorRects()
        addCursorRect(bounds, cursor: enabled ? .pointingHand : .arrow)
    }
}
private struct CursorRegion: NSViewRepresentable {
    let enabled: Bool
    func makeNSView(context: Context) -> CursorRegionView { CursorRegionView() }
    func updateNSView(_ view: CursorRegionView, context: Context) {
        view.enabled = enabled
        view.window?.invalidateCursorRects(for: view)
    }
}
struct InteractiveStyle: ButtonStyle {
    enum Surface: Equatable { case power, segment, text }
    var surface: Surface = .text
    @Environment(\.isEnabled) private var enabled
    @Environment(\.accessibilityReduceMotion) private var reduceMotion
    @State private var hovered = false

    func makeBody(configuration: Configuration) -> some View {
        configuration.label
            .overlay {
                if surface == .segment {
                    RoundedRectangle(cornerRadius: 8).fill(Color.primary.opacity(enabled && hovered ? 0.055 : 0)).allowsHitTesting(false)
                } else if surface == .text {
                    RoundedRectangle(cornerRadius: 8).fill(Color.accentColor.opacity(enabled && hovered ? 0.07 : 0)).allowsHitTesting(false)
                }
            }
            .brightness(surface == .power && enabled && hovered ? 0.035 : 0)
            .scaleEffect(enabled && configuration.isPressed ? 0.96 : 1)
            .opacity(enabled ? 1 : 0.5)
            .overlay { CursorRegion(enabled: enabled).allowsHitTesting(false).accessibilityHidden(true) }
            .onHover { hovered = $0 }
            .onContinuousHover { phase in
                guard enabled else { return }
                switch phase {
                case .active: NSCursor.pointingHand.set()
                case .ended: NSCursor.arrow.set()
                }
            }
            .onChange(of: enabled) { active in
                if !active && hovered { NSCursor.arrow.set() }
            }
            .animation(reduceMotion ? nil : .easeOut(duration: 0.14), value: hovered)
            .animation(reduceMotion ? nil : .easeOut(duration: 0.14), value: configuration.isPressed)
    }
}
