import AppKit
let destination = CommandLine.arguments[1]
let size = 1024
let rep = NSBitmapImageRep(bitmapDataPlanes: nil, pixelsWide: size, pixelsHigh: size, bitsPerSample: 8, samplesPerPixel: 4, hasAlpha: true, isPlanar: false, colorSpaceName: .deviceRGB, bytesPerRow: 0, bitsPerPixel: 0)!
NSGraphicsContext.saveGraphicsState()
NSGraphicsContext.current = NSGraphicsContext(bitmapImageRep: rep)
let bg = NSBezierPath(roundedRect: NSRect(x: 60, y: 60, width: 904, height: 904), xRadius: 205, yRadius: 205)
NSGradient(starting: NSColor(calibratedRed: 0.08, green: 0.65, blue: 0.55, alpha: 1), ending: NSColor(calibratedRed: 0.02, green: 0.32, blue: 0.32, alpha: 1))!.draw(in: bg, angle: -60)
let ring = NSBezierPath(ovalIn: NSRect(x: 260, y: 260, width: 504, height: 504))
NSColor.white.withAlphaComponent(0.17).setStroke(); ring.lineWidth = 2; ring.stroke()
let path = NSBezierPath()
path.move(to: NSPoint(x: 305, y: 402)); path.curve(to: NSPoint(x: 515, y: 622), controlPoint1: NSPoint(x: 415, y: 402), controlPoint2: NSPoint(x: 405, y: 622)); path.curve(to: NSPoint(x: 725, y: 402), controlPoint1: NSPoint(x: 625, y: 622), controlPoint2: NSPoint(x: 615, y: 402))
path.lineWidth = 53; path.lineCapStyle = .round; NSColor.white.setStroke(); path.stroke()
NSColor.white.setFill(); NSBezierPath(ovalIn: NSRect(x: 485, y: 357, width: 60, height: 60)).fill()
NSGraphicsContext.restoreGraphicsState()
try rep.representation(using: .png, properties: [:])!.write(to: URL(fileURLWithPath: destination))
