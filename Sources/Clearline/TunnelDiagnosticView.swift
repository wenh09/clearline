import SwiftUI

struct TunnelDiagnosticView: View {
    @EnvironmentObject var model: AppModel
    @Environment(\.dismiss) private var dismiss
    var body: some View {
        VStack(alignment: .leading, spacing: 16) {
            Text("检查 DNS / UDP 隧道").font(.title2.weight(.semibold))
            Text("通过已保存的原服务器测试临时虚拟网卡。不修改系统 DNS、默认路由或当前代理设置。只有检测通过后才尝试全局模式。")
                .fixedSize(horizontal: false, vertical: true)
            if model.diagnosticRunning { HStack { ProgressView().controlSize(.small); Text(model.diagnosticStageTitle) } }
            Text(model.diagnosticMessage).foregroundStyle(model.diagnosticPassed == false ? .orange : .secondary)
                .fixedSize(horizontal: false, vertical: true).textSelection(.enabled)
            HStack {
                Button(model.diagnosticRunning ? "停止检测" : "关闭") {
                    if model.diagnosticRunning { model.stopTunnelDiagnostic() } else { dismiss() }
                }
                Spacer()
                Button("开始检测") { model.startTunnelDiagnostic() }.disabled(model.diagnosticRunning || model.busy || model.packetTunnelActive)
                    .buttonStyle(.borderedProminent)
            }
        }.padding(24).frame(width: 480)
    }
}
