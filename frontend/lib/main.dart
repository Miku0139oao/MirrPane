import 'dart:async';
import 'dart:io';
import 'dart:typed_data';

import 'package:flutter/material.dart';
import 'package:flutter/services.dart';

import 'receiver_api.dart';
import 'ui_prefs.dart';

/// The shared-memory and Win32 window bridge (`dev.yingping/frames`).
///
///   'T' -> texture id
///   'F' -> pictures the engine has actually taken
///   'W' -> toggle (or set with byte 1) borderless fullscreen
///   'P' -> toggle (or set with byte 1) always-on-top
class FrameBridge {
  static const _channel = BasicMessageChannel<ByteData?>(
    'dev.yingping/frames',
    BinaryCodec(),
  );

  static Future<int> _sendCommand(int cmd, {bool? flag}) async {
    try {
      final data = flag == null ? ByteData(1) : ByteData(2);
      data.setUint8(0, cmd);
      if (flag != null) data.setUint8(1, flag ? 1 : 0);
      final reply =
          await _channel.send(data).timeout(const Duration(milliseconds: 500));
      if (reply != null && reply.lengthInBytes >= 8) {
        return reply.getInt64(0, Endian.host);
      }
    } catch (_) {
      // Ignored; callers treat negative values as unavailable.
    }
    return -1;
  }

  static Future<int> presented() => _sendCommand(0x46);

  static Future<bool> toggleFullscreen({bool? enable}) async =>
      (await _sendCommand(0x57, flag: enable)) == 1;

  static Future<bool> toggleAlwaysOnTop({bool? enable}) async =>
      (await _sendCommand(0x50, flag: enable)) == 1;

  static Future<int> textureId() async {
    for (var attempt = 0; attempt < 40; attempt++) {
      final id = await _sendCommand(0x54);
      if (id >= 0) return id;
      await Future<void>.delayed(const Duration(milliseconds: 100));
    }
    return -1;
  }
}

void main() {
  runApp(const MirrPaneApp());
}

// --- design tokens ----------------------------------------------------------

const ground = Color(0xFF191817);
const surface = Color(0xFF211F1D);
const surfaceHi = Color(0xFF2C2926);
const line = Color(0x1AFFFFFF);
const ink = Color(0xFFF3EFE9);
const muted = Color(0xFFA6A099);
const iconInk = Color(0xFFD6D0C8);
const accent = Color(0xFFD08A3E);
const okGreen = Color(0xFF6BBF7B);
const danger = Color(0xFFD4685C);

const mono = TextStyle(
  fontFamily: 'Consolas',
  fontFamilyFallback: ['Courier New', 'monospace'],
  fontFeatures: [FontFeature.tabularFigures()],
);

class MirrPaneApp extends StatelessWidget {
  const MirrPaneApp({super.key});

  @override
  Widget build(BuildContext context) {
    return MaterialApp(
      title: '鏡屏 MirrPane',
      debugShowCheckedModeBanner: false,
      theme: ThemeData(
        useMaterial3: true,
        brightness: Brightness.dark,
        scaffoldBackgroundColor: ground,
        colorScheme: const ColorScheme.dark(
          surface: surface,
          primary: accent,
          onPrimary: ground,
          onSurface: ink,
        ),
        tooltipTheme: TooltipThemeData(
          decoration: BoxDecoration(
            color: surfaceHi,
            borderRadius: BorderRadius.circular(6),
            border: Border.all(color: line),
          ),
          textStyle: const TextStyle(fontSize: 12, color: ink),
          padding: const EdgeInsets.symmetric(horizontal: 10, vertical: 6),
          waitDuration: const Duration(milliseconds: 350),
        ),
        textTheme: const TextTheme().apply(bodyColor: ink, displayColor: ink),
      ),
      home: const ReceiverScreen(),
    );
  }
}

/// ReceiverScreen owns the sidecar, window state, persistent UI prefs, and shortcuts.
class ReceiverScreen extends StatefulWidget {
  const ReceiverScreen({super.key});

  @override
  State<ReceiverScreen> createState() => _ReceiverScreenState();
}

class _ReceiverScreenState extends State<ReceiverScreen> {
  late final String _installPath;
  late ReceiverHost _host;
  int _textureId = -1;
  int _fps = 0;
  int _lastPresented = -1;

  // Interactive viewer state & persisted preferences.
  bool _fullscreen = false;
  bool _alwaysOnTop = false;
  int _quarterTurns = 0; // 0, 1, 2, 3 -> 0°, 90°, 180°, 270°
  UiPrefs _prefs = const UiPrefs();
  bool _audioSynced = false;

  // Toast banner for snapshot / copy / action feedback.
  String? _toastText;
  String? _toastOpenFolder;
  Timer? _toastTimer;
  Timer? _fpsTimer;

  @override
  void initState() {
    super.initState();
    final exeDir = File(Platform.resolvedExecutable).parent;
    _installPath = exeDir.path;
    _host = ReceiverHost(exeDir);
    _host.notifier.addListener(_onHostChanged);
    unawaited(_init());
    _fpsTimer = Timer.periodic(const Duration(seconds: 1), (_) => _sampleFps());
  }

  Future<void> _init() async {
    final loaded = await UiPrefs.load(_installPath);
    if (mounted) {
      setState(() => _prefs = loaded);
    }
    unawaited(_host.start());
    unawaited(_openTexture());
  }

  void _onHostChanged() {
    if (_audioSynced) return;
    final p = _host.notifier.value.phase;
    if (p != Phase.booting && p != Phase.failed) {
      _audioSynced = true;
      unawaited(_host.setAudio(isMuted: _prefs.muted, volume: _prefs.volume));
    }
  }

  void _updatePrefs(UiPrefs next) {
    setState(() => _prefs = next);
    unawaited(next.save(_installPath));
  }

  Future<void> _sampleFps() async {
    final n = await FrameBridge.presented();
    if (!mounted || n < 0) return;
    if (_lastPresented >= 0 && n >= _lastPresented) {
      final fps = n - _lastPresented;
      if (fps != _fps) setState(() => _fps = fps);
    }
    _lastPresented = n;
  }

  Future<void> _restart() async {
    _host.notifier.removeListener(_onHostChanged);
    await _host.stop();
    if (!mounted) return;
    _audioSynced = false;
    final nextHost = ReceiverHost(Directory(_installPath));
    nextHost.notifier.addListener(_onHostChanged);
    setState(() {
      _host = nextHost;
    });
    await _host.start();
    _showToast('接收器已重新啟動');
  }

  Future<void> _openTexture() async {
    final id = await FrameBridge.textureId();
    if (mounted) setState(() => _textureId = id);
  }

  Future<void> _toggleFullscreen({bool? enable}) async {
    final next = await FrameBridge.toggleFullscreen(enable: enable);
    if (mounted) setState(() => _fullscreen = next);
  }

  Future<void> _toggleAlwaysOnTop() async {
    final next = await FrameBridge.toggleAlwaysOnTop();
    if (mounted) {
      setState(() => _alwaysOnTop = next);
      _showToast(next ? '視窗已置頂' : '已取消視窗置頂');
    }
  }

  void _rotateRight() {
    setState(() => _quarterTurns = (_quarterTurns + 1) % 4);
    _showToast('畫面旋轉 ${_quarterTurns * 90}°');
  }

  void _toggleFlip() {
    final next = !_prefs.flipHorizontal;
    _updatePrefs(_prefs.copyWith(flipHorizontal: next));
    _showToast(next ? '已開啟水平鏡像翻轉' : '已關閉水平鏡像翻轉');
  }

  void _toggleFill() {
    final next = !_prefs.fillViewport;
    _updatePrefs(_prefs.copyWith(fillViewport: next));
    _showToast(next ? '縮放模式：填滿視窗' : '縮放模式：完整比例');
  }

  void _toggleHud() {
    _updatePrefs(_prefs.copyWith(showHud: !_prefs.showHud));
  }

  Future<void> _toggleMute() async {
    final next = !_prefs.muted;
    _updatePrefs(_prefs.copyWith(muted: next));
    await _host.setAudio(isMuted: next);
    _showToast(next ? '已靜音' : '已恢復聲音');
  }

  Future<void> _setVolume(double v) async {
    final clamped = v.clamp(0.0, 1.0);
    final muteNow = clamped <= 0.001;
    _updatePrefs(_prefs.copyWith(volume: clamped, muted: muteNow));
    await _host.setAudio(volume: clamped, isMuted: muteNow);
  }

  Future<void> _takeSnapshot() async {
    final path = await _host.takeSnapshot();
    if (!mounted) return;
    if (path != null) {
      final name = File(path).uri.pathSegments.last;
      _showToast('已儲存截圖：$name', openFolder: _host.screenshotsDir);
    } else {
      _showToast('目前沒有可擷取的畫面');
    }
  }

  Future<void> _disconnect() async {
    await _host.disconnect();
    if (_fullscreen) {
      await _toggleFullscreen(enable: false);
    }
    _showToast('已中斷連線，等待重新連接');
  }

  Future<void> _openFolder(String path) async {
    final dir = Directory(path);
    if (!await dir.exists()) {
      await dir.create(recursive: true);
    }
    unawaited(Process.run('explorer.exe', [dir.path]));
  }

  void _showToast(String text, {String? openFolder}) {
    _toastTimer?.cancel();
    setState(() {
      _toastText = text;
      _toastOpenFolder = openFolder;
    });
    _toastTimer = Timer(const Duration(seconds: 3), () {
      if (mounted) {
        setState(() {
          _toastText = null;
          _toastOpenFolder = null;
        });
      }
    });
  }

  void _openLogs() {
    showDialog<void>(
      context: context,
      builder: (_) => _LogDialog(host: _host),
    );
  }

  void _openSettings(ReceiverState? state) {
    showDialog<void>(
      context: context,
      builder: (_) => _SettingsDialog(
        host: _host,
        state: state,
        initialPrefs: _prefs,
        onPrefsChanged: _updatePrefs,
        onMuteChanged: (m) {
          _updatePrefs(_prefs.copyWith(muted: m));
          unawaited(_host.setAudio(isMuted: m));
        },
        onVolumeChanged: (v) => unawaited(_setVolume(v)),
        onOpenScreenshots: () => unawaited(_openFolder(_host.screenshotsDir)),
        onRestart: _restart,
      ),
    );
  }

  @override
  void dispose() {
    _fpsTimer?.cancel();
    _toastTimer?.cancel();
    _host.notifier.removeListener(_onHostChanged);
    unawaited(_host.stop());
    super.dispose();
  }

  @override
  Widget build(BuildContext context) {
    return ValueListenableBuilder<Snapshot>(
      valueListenable: _host.notifier,
      builder: (context, snapshot, _) {
        final state = snapshot.state;
        final isStreaming =
            snapshot.phase == Phase.live || snapshot.phase == Phase.interrupted;

        return CallbackShortcuts(
          bindings: {
            const SingleActivator(LogicalKeyboardKey.f11): () =>
                unawaited(_toggleFullscreen()),
            const SingleActivator(LogicalKeyboardKey.escape): () {
              if (_fullscreen) unawaited(_toggleFullscreen(enable: false));
            },
            const SingleActivator(LogicalKeyboardKey.keyT, control: true): () =>
                unawaited(_toggleAlwaysOnTop()),
            const SingleActivator(LogicalKeyboardKey.keyL, control: true):
                _openLogs,
            const SingleActivator(LogicalKeyboardKey.comma, control: true):
                () => _openSettings(state),
            const SingleActivator(LogicalKeyboardKey.keyS, control: true): () {
              if (isStreaming) unawaited(_takeSnapshot());
            },
            const SingleActivator(LogicalKeyboardKey.keyR): () {
              if (isStreaming) _rotateRight();
            },
            const SingleActivator(LogicalKeyboardKey.keyH): () {
              if (isStreaming) _toggleFlip();
            },
            const SingleActivator(LogicalKeyboardKey.keyZ): () {
              if (isStreaming) _toggleFill();
            },
            const SingleActivator(LogicalKeyboardKey.keyM): () {
              if (isStreaming) unawaited(_toggleMute());
            },
            const SingleActivator(LogicalKeyboardKey.tab): () {
              if (isStreaming) _toggleHud();
            },
          },
          child: Focus(
            autofocus: true,
            child: Scaffold(
              body: Stack(
                children: [
                  Positioned.fill(
                    child: AnimatedSwitcher(
                      duration: const Duration(milliseconds: 260),
                      child: _viewFor(snapshot),
                    ),
                  ),
                  if (_toastText != null)
                    Positioned(
                      left: 0,
                      right: 0,
                      bottom: 24,
                      child: Center(
                        child: _ToastPill(
                          text: _toastText!,
                          onOpenFolder: _toastOpenFolder == null
                              ? null
                              : () => unawaited(_openFolder(_toastOpenFolder!)),
                        ),
                      ),
                    ),
                ],
              ),
            ),
          ),
        );
      },
    );
  }

  Widget _viewFor(Snapshot snapshot) {
    switch (snapshot.phase) {
      case Phase.booting:
        return _BootingView(
          key: const ValueKey('booting'),
          message: snapshot.message,
        );
      case Phase.waiting:
      case Phase.connecting:
        return _WaitingView(
          key: const ValueKey('waiting'),
          state: snapshot.state,
          connecting: snapshot.phase == Phase.connecting,
          fullscreen: _fullscreen,
          alwaysOnTop: _alwaysOnTop,
          onCopyIp: (text) {
            unawaited(Clipboard.setData(ClipboardData(text: text)));
            _showToast('已複製位址：$text');
          },
          onRestart: () => unawaited(_restart()),
          onOpenScreenshots: () => unawaited(_openFolder(_host.screenshotsDir)),
          onToggleFullscreen: () => unawaited(_toggleFullscreen()),
          onToggleAlwaysOnTop: () => unawaited(_toggleAlwaysOnTop()),
          onOpenLogs: _openLogs,
          onOpenSettings: () => _openSettings(snapshot.state),
        );
      case Phase.live:
      case Phase.interrupted:
        return _LiveView(
          key: ValueKey(
              snapshot.phase == Phase.interrupted ? 'interrupted' : 'live'),
          textureId: _textureId,
          state: snapshot.state,
          fps: _fps,
          interrupted: snapshot.phase == Phase.interrupted,
          fullscreen: _fullscreen,
          alwaysOnTop: _alwaysOnTop,
          quarterTurns: _quarterTurns,
          prefs: _prefs,
          onRotate: _rotateRight,
          onToggleFlip: _toggleFlip,
          onToggleFill: _toggleFill,
          onToggleHud: _toggleHud,
          onSnapshot: () => unawaited(_takeSnapshot()),
          onToggleMute: () => unawaited(_toggleMute()),
          onVolumeChanged: (v) => unawaited(_setVolume(v)),
          onDisconnect: () => unawaited(_disconnect()),
          onToggleFullscreen: () => unawaited(_toggleFullscreen()),
          onExitFullscreen: () => unawaited(_toggleFullscreen(enable: false)),
          onToggleAlwaysOnTop: () => unawaited(_toggleAlwaysOnTop()),
          onOpenLogs: _openLogs,
          onOpenSettings: () => _openSettings(snapshot.state),
        );
      case Phase.failed:
        return _ErrorView(
          key: const ValueKey('failed'),
          message: snapshot.error ?? '接收器無法啟動',
          host: _host,
          onRetry: _restart,
        );
    }
  }
}

// --- views ------------------------------------------------------------------

class _AppLogo extends StatelessWidget {
  const _AppLogo({this.size = 28});

  final double size;

  @override
  Widget build(BuildContext context) {
    return ClipRRect(
      borderRadius: BorderRadius.circular(size * 0.225),
      child: Image.asset(
        'assets/logo.png',
        width: size,
        height: size,
        filterQuality: FilterQuality.high,
      ),
    );
  }
}

class _BootingView extends StatelessWidget {
  const _BootingView({super.key, this.message});

  final String? message;

  @override
  Widget build(BuildContext context) {
    return Center(
      child: Column(
        mainAxisSize: MainAxisSize.min,
        children: [
          const _AppLogo(size: 64),
          const SizedBox(height: 20),
          const SizedBox(
            width: 22,
            height: 22,
            child: CircularProgressIndicator(strokeWidth: 2.2, color: accent),
          ),
          const SizedBox(height: 18),
          const Text(
            '正在啟動 鏡屏 MirrPane',
            style: TextStyle(fontSize: 22, fontWeight: FontWeight.w500),
          ),
          const SizedBox(height: 8),
          Padding(
            padding: const EdgeInsets.symmetric(horizontal: 24),
            child: Text(
              message ?? '正在初始化解碼引擎與 AirPlay 廣播…',
              textAlign: TextAlign.center,
              style: const TextStyle(fontSize: 13, color: muted),
            ),
          ),
        ],
      ),
    );
  }
}

class _WaitingView extends StatelessWidget {
  const _WaitingView({
    super.key,
    required this.state,
    required this.connecting,
    required this.fullscreen,
    required this.alwaysOnTop,
    required this.onCopyIp,
    required this.onRestart,
    required this.onOpenScreenshots,
    required this.onToggleFullscreen,
    required this.onToggleAlwaysOnTop,
    required this.onOpenLogs,
    required this.onOpenSettings,
  });

  final ReceiverState? state;
  final bool connecting;
  final bool fullscreen;
  final bool alwaysOnTop;
  final ValueChanged<String> onCopyIp;
  final VoidCallback onRestart;
  final VoidCallback onOpenScreenshots;
  final VoidCallback onToggleFullscreen;
  final VoidCallback onToggleAlwaysOnTop;
  final VoidCallback onOpenLogs;
  final VoidCallback onOpenSettings;

  @override
  Widget build(BuildContext context) {
    final name = state?.name.isNotEmpty == true ? state!.name : '鏡屏';
    final ip = state?.ip ?? '';
    final port = state?.port ?? 7000;
    final addr = ip.isEmpty ? '' : '$ip:$port';
    final targetRes = state != null
        ? '${state!.targetWidth}×${state!.targetHeight}@${state!.maxFps}'
        : null;

    return SafeArea(
      child: Padding(
        padding: const EdgeInsets.symmetric(horizontal: 56, vertical: 34),
        child: Column(
          crossAxisAlignment: CrossAxisAlignment.start,
          children: [
            Row(
              children: [
                const _AppLogo(size: 28),
                const SizedBox(width: 12),
                _Broadcasting(connecting: connecting),
                const Spacer(),
                _IconToolButton(
                  icon: Icons.photo_library_outlined,
                  tooltip: '開啟截圖資料夾',
                  onPressed: onOpenScreenshots,
                ),
                const SizedBox(width: 6),
                _IconToolButton(
                  icon: Icons.refresh,
                  tooltip: '重新廣播',
                  onPressed: onRestart,
                ),
                const SizedBox(width: 6),
                _IconToolButton(
                  icon: alwaysOnTop ? Icons.push_pin : Icons.push_pin_outlined,
                  tooltip: '視窗置頂 (Ctrl+T)',
                  active: alwaysOnTop,
                  onPressed: onToggleAlwaysOnTop,
                ),
                const SizedBox(width: 6),
                _IconToolButton(
                  icon: fullscreen ? Icons.fullscreen_exit : Icons.fullscreen,
                  tooltip: '全螢幕 (F11)',
                  active: fullscreen,
                  onPressed: onToggleFullscreen,
                ),
                const SizedBox(width: 6),
                _IconToolButton(
                  icon: Icons.article_outlined,
                  tooltip: '執行日誌 (Ctrl+L)',
                  onPressed: onOpenLogs,
                ),
                const SizedBox(width: 6),
                _IconToolButton(
                  icon: Icons.settings_outlined,
                  tooltip: '設定 (Ctrl+,)',
                  onPressed: onOpenSettings,
                ),
              ],
            ),
            const Spacer(flex: 2),
            Text(
              name,
              style: const TextStyle(
                fontSize: 64,
                height: 1.06,
                fontWeight: FontWeight.w500,
                letterSpacing: -1.6,
              ),
            ),
            const SizedBox(height: 16),
            Wrap(
              spacing: 10,
              runSpacing: 8,
              crossAxisAlignment: WrapCrossAlignment.center,
              children: [
                Material(
                  color: surface,
                  borderRadius: BorderRadius.circular(6),
                  child: InkWell(
                    onTap: addr.isEmpty ? null : () => onCopyIp(addr),
                    borderRadius: BorderRadius.circular(6),
                    child: Container(
                      padding: const EdgeInsets.symmetric(
                          horizontal: 10, vertical: 6),
                      decoration: BoxDecoration(
                        borderRadius: BorderRadius.circular(6),
                        border: Border.all(color: line),
                      ),
                      child: Row(
                        mainAxisSize: MainAxisSize.min,
                        children: [
                          Text(
                            addr.isEmpty ? '本機網路' : addr,
                            style: mono.copyWith(fontSize: 13, color: ink),
                          ),
                          if (addr.isNotEmpty) ...[
                            const SizedBox(width: 8),
                            const Icon(Icons.copy, size: 13, color: muted),
                          ],
                        ],
                      ),
                    ),
                  ),
                ),
                if (targetRes != null) _Badge(targetRes),
                if (state?.decoderMode == 'software')
                  const _Badge('CPU 軟解', accentColor: accent)
                else if (state?.decoder.isNotEmpty == true)
                  _Badge(state!.decoder,
                      accentColor:
                          state!.decoder == 'D3D11VA' ? okGreen : muted),
                if (state?.version.isNotEmpty == true)
                  _Badge('AirPlay ${state!.version}'),
              ],
            ),
            const Spacer(flex: 3),
            Container(
              constraints: const BoxConstraints(maxWidth: 620),
              padding: const EdgeInsets.all(20),
              decoration: BoxDecoration(
                color: surface,
                borderRadius: BorderRadius.circular(10),
                border: Border.all(color: line),
              ),
              child: Row(
                children: [
                  Container(
                    width: 42,
                    height: 42,
                    decoration: BoxDecoration(
                      color: (connecting ? okGreen : accent)
                          .withValues(alpha: 0.14),
                      borderRadius: BorderRadius.circular(8),
                    ),
                    child: Icon(
                      connecting ? Icons.sync : Icons.cast_connected,
                      color: connecting ? okGreen : accent,
                      size: 21,
                    ),
                  ),
                  const SizedBox(width: 16),
                  Expanded(
                    child: Text(
                      connecting
                          ? '正在與裝置完成 AirPlay 配對與串流交握…'
                          : '在 iPhone 或 iPad 開啟「控制中心」→「螢幕鏡像輸出」，選擇「$name」。',
                      style: const TextStyle(
                          fontSize: 14, height: 1.55, color: ink),
                    ),
                  ),
                ],
              ),
            ),
            const SizedBox(height: 20),
            Text(
              'F11 全螢幕  ·  R 旋轉  ·  H 翻轉  ·  Z 填滿/比例  ·  M 靜音  ·  Ctrl+S 截圖  ·  Tab 效能監控',
              style: mono.copyWith(fontSize: 11.5, color: muted),
            ),
          ],
        ),
      ),
    );
  }
}

class _Badge extends StatelessWidget {
  const _Badge(this.text, {this.accentColor});

  final String text;
  final Color? accentColor;

  @override
  Widget build(BuildContext context) {
    final c = accentColor ?? muted;
    return Container(
      padding: const EdgeInsets.symmetric(horizontal: 9, vertical: 5),
      decoration: BoxDecoration(
        color: surface,
        borderRadius: BorderRadius.circular(6),
        border: Border.all(color: line),
      ),
      child: Text(text, style: mono.copyWith(fontSize: 11.5, color: c)),
    );
  }
}

class _Broadcasting extends StatelessWidget {
  const _Broadcasting({required this.connecting});

  final bool connecting;

  @override
  Widget build(BuildContext context) {
    return Row(
      children: [
        _StatusDot(color: connecting ? okGreen : accent),
        const SizedBox(width: 10),
        Text(
          connecting ? '正在連線中…' : '等待連線',
          style: TextStyle(
            fontSize: 13,
            color: connecting ? ink : muted,
            letterSpacing: 0.2,
          ),
        ),
      ],
    );
  }
}

class _StatusDot extends StatefulWidget {
  const _StatusDot({this.color = accent});

  final Color color;

  @override
  State<_StatusDot> createState() => _StatusDotState();
}

class _StatusDotState extends State<_StatusDot>
    with SingleTickerProviderStateMixin {
  late final AnimationController _c = AnimationController(
    vsync: this,
    duration: const Duration(milliseconds: 2200),
  )..repeat(reverse: true);

  @override
  void dispose() {
    _c.dispose();
    super.dispose();
  }

  @override
  Widget build(BuildContext context) {
    return FadeTransition(
      opacity: Tween(begin: 0.35, end: 1.0).animate(
        CurvedAnimation(parent: _c, curve: Curves.easeInOut),
      ),
      child: Container(
        width: 8,
        height: 8,
        decoration: BoxDecoration(color: widget.color, shape: BoxShape.circle),
      ),
    );
  }
}

class _LiveView extends StatefulWidget {
  const _LiveView({
    super.key,
    required this.textureId,
    required this.state,
    required this.fps,
    required this.interrupted,
    required this.fullscreen,
    required this.alwaysOnTop,
    required this.quarterTurns,
    required this.prefs,
    required this.onRotate,
    required this.onToggleFlip,
    required this.onToggleFill,
    required this.onToggleHud,
    required this.onSnapshot,
    required this.onToggleMute,
    required this.onVolumeChanged,
    required this.onDisconnect,
    required this.onToggleFullscreen,
    required this.onExitFullscreen,
    required this.onToggleAlwaysOnTop,
    required this.onOpenLogs,
    required this.onOpenSettings,
  });

  final int textureId;
  final ReceiverState? state;
  final int fps;
  final bool interrupted;
  final bool fullscreen;
  final bool alwaysOnTop;
  final int quarterTurns;
  final UiPrefs prefs;
  final VoidCallback onRotate;
  final VoidCallback onToggleFlip;
  final VoidCallback onToggleFill;
  final VoidCallback onToggleHud;
  final VoidCallback onSnapshot;
  final VoidCallback onToggleMute;
  final ValueChanged<double> onVolumeChanged;
  final VoidCallback onDisconnect;
  final VoidCallback onToggleFullscreen;
  final VoidCallback onExitFullscreen;
  final VoidCallback onToggleAlwaysOnTop;
  final VoidCallback onOpenLogs;
  final VoidCallback onOpenSettings;

  @override
  State<_LiveView> createState() => _LiveViewState();
}

class _LiveViewState extends State<_LiveView> {
  bool _chrome = true;
  bool _barHovered = false;
  Timer? _hide;

  @override
  void initState() {
    super.initState();
    _scheduleHide();
  }

  @override
  void didUpdateWidget(covariant _LiveView oldWidget) {
    super.didUpdateWidget(oldWidget);
    if (oldWidget.prefs.barHideSeconds != widget.prefs.barHideSeconds) {
      if (widget.prefs.barHideSeconds == 0) {
        _hide?.cancel();
        if (!_chrome) setState(() => _chrome = true);
      } else {
        _scheduleHide();
      }
    }
  }

  @override
  void dispose() {
    _hide?.cancel();
    super.dispose();
  }

  void _scheduleHide() {
    _hide?.cancel();
    final secs = widget.prefs.barHideSeconds;
    if (secs <= 0) {
      if (!_chrome) setState(() => _chrome = true);
      return;
    }
    _hide = Timer(Duration(seconds: secs), () {
      if (mounted && !_barHovered && !widget.interrupted) {
        setState(() => _chrome = false);
      }
    });
  }

  void _reveal() {
    if (!_chrome) setState(() => _chrome = true);
    _scheduleHide();
  }

  Widget _buildVideoSurface() {
    final s = widget.state;
    final rawW = (s != null && s.width > 0) ? s.width.toDouble() : 16.0;
    final rawH = (s != null && s.height > 0) ? s.height.toDouble() : 9.0;
    final oddTurn = widget.quarterTurns.isOdd;
    final displayW = oddTurn ? rawH : rawW;
    final displayH = oddTurn ? rawW : rawH;

    Widget frame = widget.textureId >= 0
        ? Texture(
            textureId: widget.textureId,
            filterQuality: widget.prefs.toFlutterFilterQuality(),
          )
        : const SizedBox.expand();

    final cf = widget.prefs.buildColorFilter();
    if (cf != null) {
      frame = ColorFiltered(colorFilter: cf, child: frame);
    }
    if (widget.prefs.flipHorizontal) {
      frame = Transform.flip(flipX: true, child: frame);
    }

    final rotated = RotatedBox(
      quarterTurns: widget.quarterTurns,
      child: SizedBox(width: rawW, height: rawH, child: frame),
    );

    return FittedBox(
      fit: widget.prefs.fillViewport ? BoxFit.cover : BoxFit.contain,
      clipBehavior: Clip.hardEdge,
      child: SizedBox(width: displayW, height: displayH, child: rotated),
    );
  }

  Widget _buildHud(bool showTopBar) {
    final topOffset = showTopBar ? 68.0 : 16.0;
    final hud = _StatsHud(state: widget.state, fps: widget.fps);
    switch (widget.prefs.hudCorner) {
      case 'topLeft':
        return Positioned(left: 16, top: topOffset, child: hud);
      case 'topRight':
        return Positioned(right: 16, top: topOffset, child: hud);
      case 'bottomRight':
        return Positioned(right: 16, bottom: 16, child: hud);
      default:
        return Positioned(left: 16, bottom: 16, child: hud);
    }
  }

  @override
  Widget build(BuildContext context) {
    final showTopBar =
        _chrome || widget.interrupted || widget.prefs.barHideSeconds == 0;

    return MouseRegion(
      onHover: (_) => _reveal(),
      onExit: (_) => _scheduleHide(),
      child: ColoredBox(
        color: Colors.black,
        child: Stack(
          fit: StackFit.expand,
          children: [
            Positioned.fill(
              child: GestureDetector(
                behavior: HitTestBehavior.opaque,
                onDoubleTap: widget.onToggleFullscreen,
                child: _buildVideoSurface(),
              ),
            ),
            if (widget.prefs.showHud && !widget.interrupted)
              _buildHud(showTopBar),
            if (widget.interrupted)
              _InterruptedCard(
                reason: widget.state?.reason ?? '',
                fullscreen: widget.fullscreen,
                onReturnWaiting: widget.onDisconnect,
                onExitFullscreen: widget.onExitFullscreen,
              ),
            Positioned(
              left: 0,
              right: 0,
              top: 0,
              child: MouseRegion(
                onEnter: (_) {
                  _barHovered = true;
                  _reveal();
                },
                onExit: (_) {
                  _barHovered = false;
                  _scheduleHide();
                },
                child: IgnorePointer(
                  ignoring: !showTopBar,
                  child: AnimatedOpacity(
                    opacity: showTopBar ? 1 : 0,
                    duration: const Duration(milliseconds: 200),
                    child: _TopBar(
                      state: widget.state,
                      fps: widget.fps,
                      interrupted: widget.interrupted,
                      fullscreen: widget.fullscreen,
                      alwaysOnTop: widget.alwaysOnTop,
                      flipHorizontal: widget.prefs.flipHorizontal,
                      fillViewport: widget.prefs.fillViewport,
                      showHud: widget.prefs.showHud,
                      isMuted: widget.prefs.muted,
                      volume: widget.prefs.volume,
                      onRotate: widget.onRotate,
                      onToggleFlip: widget.onToggleFlip,
                      onToggleFill: widget.onToggleFill,
                      onToggleHud: widget.onToggleHud,
                      onSnapshot: widget.onSnapshot,
                      onToggleMute: widget.onToggleMute,
                      onVolumeChanged: widget.onVolumeChanged,
                      onDisconnect: widget.onDisconnect,
                      onToggleFullscreen: widget.onToggleFullscreen,
                      onToggleAlwaysOnTop: widget.onToggleAlwaysOnTop,
                      onOpenLogs: widget.onOpenLogs,
                      onOpenSettings: widget.onOpenSettings,
                    ),
                  ),
                ),
              ),
            ),
          ],
        ),
      ),
    );
  }
}

class _StatsHud extends StatelessWidget {
  const _StatsHud({required this.state, required this.fps});

  final ReceiverState? state;
  final int fps;

  @override
  Widget build(BuildContext context) {
    final s = state;
    final res = (s != null && s.width > 0) ? '${s.width}×${s.height}' : '—';
    final dec = (s?.decoder.isNotEmpty == true) ? s!.decoder : '—';
    final dropped = s?.dropped ?? 0;

    return IgnorePointer(
      child: Container(
        padding: const EdgeInsets.symmetric(horizontal: 12, vertical: 8),
        decoration: BoxDecoration(
          color: const Color(0xCC191817),
          borderRadius: BorderRadius.circular(8),
          border: Border.all(color: line),
        ),
        child: Row(
          mainAxisSize: MainAxisSize.min,
          children: [
            Text('$fps fps',
                style: mono.copyWith(
                    fontSize: 12, color: okGreen, fontWeight: FontWeight.w600)),
            const SizedBox(width: 10),
            Text(res, style: mono.copyWith(fontSize: 12, color: ink)),
            const SizedBox(width: 10),
            Text(dec, style: mono.copyWith(fontSize: 11.5, color: muted)),
            if (dropped > 0) ...[
              const SizedBox(width: 10),
              Text('略過 $dropped',
                  style: mono.copyWith(fontSize: 11.5, color: accent)),
            ],
          ],
        ),
      ),
    );
  }
}

class _InterruptedCard extends StatelessWidget {
  const _InterruptedCard({
    required this.reason,
    required this.fullscreen,
    required this.onReturnWaiting,
    required this.onExitFullscreen,
  });

  final String reason;
  final bool fullscreen;
  final VoidCallback onReturnWaiting;
  final VoidCallback onExitFullscreen;

  @override
  Widget build(BuildContext context) {
    return Container(
      color: const Color(0x99000000),
      alignment: Alignment.center,
      child: Container(
        width: 460,
        padding: const EdgeInsets.all(28),
        decoration: BoxDecoration(
          color: const Color(0xF2211F1D),
          borderRadius: BorderRadius.circular(14),
          border: Border.all(color: line),
        ),
        child: Column(
          mainAxisSize: MainAxisSize.min,
          crossAxisAlignment: CrossAxisAlignment.start,
          children: [
            const Row(
              children: [
                Icon(Icons.wifi_off_rounded, color: accent, size: 22),
                SizedBox(width: 10),
                Text('連線已中斷',
                    style:
                        TextStyle(fontSize: 22, fontWeight: FontWeight.w500)),
              ],
            ),
            const SizedBox(height: 10),
            Text(
              reason.isEmpty
                  ? '手機的畫面串流已停止，接收器仍在等待重新連線。'
                  : '手機的畫面串流已停止（$reason），你可以直接從手機重連或返回等待畫面。',
              style: const TextStyle(fontSize: 13.5, height: 1.5, color: muted),
            ),
            const SizedBox(height: 22),
            Row(
              children: [
                FilledButton.icon(
                  onPressed: onReturnWaiting,
                  style: FilledButton.styleFrom(
                    backgroundColor: accent,
                    foregroundColor: ground,
                  ),
                  icon: const Icon(Icons.home_outlined, size: 17),
                  label: const Text('返回等待畫面'),
                ),
                if (fullscreen) ...[
                  const SizedBox(width: 10),
                  OutlinedButton.icon(
                    onPressed: onExitFullscreen,
                    style: OutlinedButton.styleFrom(
                      foregroundColor: ink,
                      side: const BorderSide(color: line),
                    ),
                    icon: const Icon(Icons.fullscreen_exit, size: 17),
                    label: const Text('離開全螢幕'),
                  ),
                ],
              ],
            ),
          ],
        ),
      ),
    );
  }
}

class _TopBar extends StatelessWidget {
  const _TopBar({
    required this.state,
    required this.fps,
    required this.interrupted,
    required this.fullscreen,
    required this.alwaysOnTop,
    required this.flipHorizontal,
    required this.fillViewport,
    required this.showHud,
    required this.isMuted,
    required this.volume,
    required this.onRotate,
    required this.onToggleFlip,
    required this.onToggleFill,
    required this.onToggleHud,
    required this.onSnapshot,
    required this.onToggleMute,
    required this.onVolumeChanged,
    required this.onDisconnect,
    required this.onToggleFullscreen,
    required this.onToggleAlwaysOnTop,
    required this.onOpenLogs,
    required this.onOpenSettings,
  });

  final ReceiverState? state;
  final int fps;
  final bool interrupted;
  final bool fullscreen;
  final bool alwaysOnTop;
  final bool flipHorizontal;
  final bool fillViewport;
  final bool showHud;
  final bool isMuted;
  final double volume;
  final VoidCallback onRotate;
  final VoidCallback onToggleFlip;
  final VoidCallback onToggleFill;
  final VoidCallback onToggleHud;
  final VoidCallback onSnapshot;
  final VoidCallback onToggleMute;
  final ValueChanged<double> onVolumeChanged;
  final VoidCallback onDisconnect;
  final VoidCallback onToggleFullscreen;
  final VoidCallback onToggleAlwaysOnTop;
  final VoidCallback onOpenLogs;
  final VoidCallback onOpenSettings;

  @override
  Widget build(BuildContext context) {
    final s = state;
    final size = (s != null && s.width > 0) ? '${s.width}×${s.height}' : null;
    final dropped = s?.dropped ?? 0;

    return Container(
      height: 52,
      padding: const EdgeInsets.symmetric(horizontal: 16),
      decoration: const BoxDecoration(
        color: Color(0xE0211F1D),
        border: Border(bottom: BorderSide(color: line)),
      ),
      child: Row(
        children: [
          Container(
            width: 7,
            height: 7,
            decoration: BoxDecoration(
              color: interrupted ? accent : okGreen,
              shape: BoxShape.circle,
            ),
          ),
          const SizedBox(width: 8),
          Text(
            interrupted
                ? '連線中斷'
                : (s?.sender.isNotEmpty == true ? s!.sender : '鏡像中'),
            style: const TextStyle(fontSize: 13, color: ink),
          ),
          if (size != null) ...[
            const SizedBox(width: 12),
            Text(size, style: mono.copyWith(fontSize: 12, color: muted)),
          ],
          if (fps > 0 && !interrupted) ...[
            const SizedBox(width: 12),
            Text('$fps fps', style: mono.copyWith(fontSize: 12, color: ink)),
          ],
          if (s?.decoder.isNotEmpty == true) ...[
            const SizedBox(width: 10),
            Container(
              padding: const EdgeInsets.symmetric(horizontal: 6, vertical: 2),
              decoration: BoxDecoration(
                color: surfaceHi,
                borderRadius: BorderRadius.circular(4),
              ),
              child: Text(
                s!.decoder,
                style: mono.copyWith(
                  fontSize: 10.5,
                  color: s.decoder == 'D3D11VA' ? okGreen : muted,
                ),
              ),
            ),
          ],
          if (dropped > 0) ...[
            const SizedBox(width: 10),
            Text('略過 $dropped',
                style: mono.copyWith(fontSize: 11.5, color: accent)),
          ],
          const Spacer(),
          _IconToolButton(
            icon: isMuted || volume <= 0.01
                ? Icons.volume_off_outlined
                : (volume < 0.5
                    ? Icons.volume_down_outlined
                    : Icons.volume_up_outlined),
            tooltip: isMuted ? '取消靜音 (M)' : '靜音 (M)',
            active: isMuted,
            onPressed: onToggleMute,
          ),
          SizedBox(
            width: 84,
            child: SliderTheme(
              data: SliderThemeData(
                trackHeight: 2.5,
                thumbShape: const RoundSliderThumbShape(enabledThumbRadius: 5),
                overlayShape: const RoundSliderOverlayShape(overlayRadius: 12),
                activeTrackColor: isMuted ? muted : accent,
                inactiveTrackColor: line,
                thumbColor: isMuted ? muted : ink,
              ),
              child: Slider(
                value: isMuted ? 0.0 : volume.clamp(0.0, 1.0),
                onChanged: onVolumeChanged,
              ),
            ),
          ),
          const SizedBox(width: 6),
          _BarDivider(),
          const SizedBox(width: 6),
          _IconToolButton(
            icon: Icons.rotate_right,
            tooltip: '旋轉畫面 90° (R)',
            onPressed: onRotate,
          ),
          const SizedBox(width: 2),
          _IconToolButton(
            icon: Icons.flip,
            tooltip: '水平鏡像翻轉 (H)',
            active: flipHorizontal,
            onPressed: onToggleFlip,
          ),
          const SizedBox(width: 2),
          _IconToolButton(
            icon: fillViewport ? Icons.fit_screen : Icons.aspect_ratio,
            tooltip: fillViewport ? '切換完整比例 (Z)' : '切換填滿畫面 (Z)',
            active: fillViewport,
            onPressed: onToggleFill,
          ),
          const SizedBox(width: 2),
          _IconToolButton(
            icon: Icons.camera_alt_outlined,
            tooltip: '截圖 (Ctrl+S)',
            onPressed: onSnapshot,
          ),
          const SizedBox(width: 2),
          _IconToolButton(
            icon: Icons.speed_outlined,
            tooltip: '效能監控 HUD (Tab)',
            active: showHud,
            onPressed: onToggleHud,
          ),
          const SizedBox(width: 6),
          _BarDivider(),
          const SizedBox(width: 6),
          _IconToolButton(
            icon: alwaysOnTop ? Icons.push_pin : Icons.push_pin_outlined,
            tooltip: '視窗置頂 (Ctrl+T)',
            active: alwaysOnTop,
            onPressed: onToggleAlwaysOnTop,
          ),
          const SizedBox(width: 2),
          _IconToolButton(
            icon: fullscreen ? Icons.fullscreen_exit : Icons.fullscreen,
            tooltip: fullscreen ? '離開全螢幕 (Esc)' : '全螢幕 (F11 / 雙擊)',
            active: fullscreen,
            onPressed: onToggleFullscreen,
          ),
          const SizedBox(width: 2),
          _IconToolButton(
            icon: Icons.article_outlined,
            tooltip: '執行日誌 (Ctrl+L)',
            onPressed: onOpenLogs,
          ),
          const SizedBox(width: 2),
          _IconToolButton(
            icon: Icons.settings_outlined,
            tooltip: '設定 (Ctrl+,)',
            onPressed: onOpenSettings,
          ),
          const SizedBox(width: 2),
          _IconToolButton(
            icon: Icons.link_off,
            tooltip: '中斷連線並返回等待畫面',
            dangerTone: true,
            onPressed: onDisconnect,
          ),
        ],
      ),
    );
  }
}

class _BarDivider extends StatelessWidget {
  @override
  Widget build(BuildContext context) {
    return Container(width: 1, height: 18, color: line);
  }
}

class _ToastPill extends StatelessWidget {
  const _ToastPill({required this.text, this.onOpenFolder});

  final String text;
  final VoidCallback? onOpenFolder;

  @override
  Widget build(BuildContext context) {
    return Material(
      color: const Color(0xF02B2825),
      elevation: 6,
      borderRadius: BorderRadius.circular(10),
      child: Container(
        padding: const EdgeInsets.symmetric(horizontal: 14, vertical: 9),
        decoration: BoxDecoration(
          borderRadius: BorderRadius.circular(10),
          border: Border.all(color: line),
        ),
        child: Row(
          mainAxisSize: MainAxisSize.min,
          children: [
            Text(text, style: const TextStyle(fontSize: 13, color: ink)),
            if (onOpenFolder != null) ...[
              const SizedBox(width: 12),
              TextButton(
                onPressed: onOpenFolder,
                style: TextButton.styleFrom(
                  foregroundColor: accent,
                  padding: const EdgeInsets.symmetric(horizontal: 8),
                  minimumSize: const Size(0, 28),
                ),
                child: const Text('開啟資料夾'),
              ),
            ],
          ],
        ),
      ),
    );
  }
}

class _IconToolButton extends StatelessWidget {
  const _IconToolButton({
    required this.icon,
    required this.tooltip,
    required this.onPressed,
    this.active = false,
    this.dangerTone = false,
  });

  final IconData icon;
  final String tooltip;
  final VoidCallback onPressed;
  final bool active;
  final bool dangerTone;

  @override
  Widget build(BuildContext context) {
    final fg = dangerTone ? danger : (active ? accent : iconInk);
    return Tooltip(
      message: tooltip,
      child: Material(
        type: MaterialType.transparency,
        child: InkWell(
          onTap: onPressed,
          borderRadius: BorderRadius.circular(6),
          hoverColor: Colors.white.withValues(alpha: 0.08),
          child: Container(
            width: 34,
            height: 34,
            decoration: BoxDecoration(
              color:
                  active ? accent.withValues(alpha: 0.18) : Colors.transparent,
              borderRadius: BorderRadius.circular(6),
              border: active
                  ? Border.all(color: accent.withValues(alpha: 0.45))
                  : null,
            ),
            child: Icon(icon, size: 17, color: fg),
          ),
        ),
      ),
    );
  }
}

class _LogDialog extends StatefulWidget {
  const _LogDialog({required this.host});

  final ReceiverHost host;

  @override
  State<_LogDialog> createState() => _LogDialogState();
}

class _LogDialogState extends State<_LogDialog> {
  List<String> _lines = const [];
  bool _copied = false;

  @override
  void initState() {
    super.initState();
    unawaited(_refresh());
  }

  Future<void> _refresh() async {
    final lines = await widget.host.logTail(lines: 100);
    if (mounted) setState(() => _lines = lines);
  }

  Future<void> _copy() async {
    await Clipboard.setData(ClipboardData(text: _lines.join('\n')));
    if (!mounted) return;
    setState(() => _copied = true);
    Timer(const Duration(seconds: 2), () {
      if (mounted) setState(() => _copied = false);
    });
  }

  @override
  Widget build(BuildContext context) {
    return Dialog(
      backgroundColor: surface,
      shape: RoundedRectangleBorder(
        borderRadius: BorderRadius.circular(12),
        side: const BorderSide(color: line),
      ),
      child: ConstrainedBox(
        constraints: const BoxConstraints(maxWidth: 780, maxHeight: 540),
        child: Padding(
          padding: const EdgeInsets.all(20),
          child: Column(
            crossAxisAlignment: CrossAxisAlignment.start,
            children: [
              Row(
                children: [
                  const Text('執行日誌',
                      style:
                          TextStyle(fontSize: 18, fontWeight: FontWeight.w500)),
                  const SizedBox(width: 12),
                  Expanded(
                    child: Text(
                      widget.host.logPath,
                      overflow: TextOverflow.ellipsis,
                      style: mono.copyWith(fontSize: 11.5, color: muted),
                    ),
                  ),
                  TextButton.icon(
                    onPressed: () => unawaited(_refresh()),
                    icon: const Icon(Icons.refresh, size: 16),
                    label: const Text('重新整理'),
                  ),
                  const SizedBox(width: 6),
                  TextButton.icon(
                    onPressed: _lines.isEmpty ? null : () => unawaited(_copy()),
                    icon: Icon(_copied ? Icons.check : Icons.copy, size: 16),
                    label: Text(_copied ? '已複製' : '複製'),
                  ),
                  const SizedBox(width: 6),
                  IconButton(
                    onPressed: () => Navigator.of(context).pop(),
                    icon: const Icon(Icons.close, size: 18),
                  ),
                ],
              ),
              const SizedBox(height: 12),
              Expanded(
                child: Container(
                  width: double.infinity,
                  padding: const EdgeInsets.all(14),
                  decoration: BoxDecoration(
                    color: ground,
                    borderRadius: BorderRadius.circular(8),
                    border: Border.all(color: line),
                  ),
                  child: SingleChildScrollView(
                    reverse: true,
                    child: SelectableText(
                      _lines.isEmpty ? '（目前沒有日誌）' : _lines.join('\n'),
                      style: mono.copyWith(
                          fontSize: 12, height: 1.5, color: muted),
                    ),
                  ),
                ),
              ),
            ],
          ),
        ),
      ),
    );
  }
}

class _SettingsDialog extends StatefulWidget {
  const _SettingsDialog({
    required this.host,
    required this.state,
    required this.initialPrefs,
    required this.onPrefsChanged,
    required this.onMuteChanged,
    required this.onVolumeChanged,
    required this.onOpenScreenshots,
    required this.onRestart,
  });

  final ReceiverHost host;
  final ReceiverState? state;
  final UiPrefs initialPrefs;
  final ValueChanged<UiPrefs> onPrefsChanged;
  final ValueChanged<bool> onMuteChanged;
  final ValueChanged<double> onVolumeChanged;
  final VoidCallback onOpenScreenshots;
  final Future<void> Function() onRestart;

  @override
  State<_SettingsDialog> createState() => _SettingsDialogState();
}

class _SettingsDialogState extends State<_SettingsDialog> {
  // General + backend settings (persisted to config.json, restart to apply)
  late final TextEditingController _name = TextEditingController(
    text: widget.state?.name.isNotEmpty == true ? widget.state!.name : '鏡屏',
  );
  late String _resKey = _matchResKey(
    widget.state?.targetWidth ?? 1920,
    widget.state?.targetHeight ?? 1080,
  );
  late int _maxFps = (widget.state?.maxFps == 30) ? 30 : 60;
  late String _decoderMode = widget.state?.decoderMode ?? 'auto';
  late String _colorRange = widget.state?.colorRange ?? 'auto';
  late String _scaleFilter = widget.state?.scaleFilter ?? 'fast';
  late String _audioBuffer = widget.state?.audioBuffer ?? 'balanced';
  bool _backendDirty = false;
  bool _busy = false;

  // Live frontend preferences (immediate GPU / UI effect)
  late UiPrefs _prefs = widget.initialPrefs;

  static const _resolutions = <String, (int, int, String)>{
    '1280x720': (1280, 720, '1280 × 720 (HD 720p)'),
    '1920x1080': (1920, 1080, '1920 × 1080 (Full HD · 預設)'),
    '2560x1440': (2560, 1440, '2560 × 1440 (2K QHD)'),
    '3840x2160': (3840, 2160, '3840 × 2160 (4K UHD)'),
  };

  static String _matchResKey(int w, int h) {
    final k = '${w}x$h';
    return _resolutions.containsKey(k) ? k : '1920x1080';
  }

  @override
  void dispose() {
    _name.dispose();
    super.dispose();
  }

  void _setPrefs(UiPrefs next) {
    setState(() => _prefs = next);
    widget.onPrefsChanged(next);
  }

  Future<void> _saveAndRestart() async {
    setState(() => _busy = true);
    final res = _resolutions[_resKey] ?? (1920, 1080, '');
    final trimmedName = _name.text.trim();
    final ok = await widget.host.saveConfig({
      if (trimmedName.isNotEmpty) 'name': trimmedName,
      'width': res.$1,
      'height': res.$2,
      'maxFps': _maxFps,
      'decoderMode': _decoderMode,
      'colorRange': _colorRange,
      'scaleFilter': _scaleFilter,
      'audioBuffer': _audioBuffer,
    });
    if (ok) await widget.onRestart();
    if (mounted) Navigator.of(context).pop();
  }

  Future<void> _resetIdentity() async {
    setState(() => _busy = true);
    await widget.host.resetIdentity();
    await widget.onRestart();
    if (mounted) Navigator.of(context).pop();
  }

  @override
  Widget build(BuildContext context) {
    return Dialog(
      backgroundColor: surface,
      shape: RoundedRectangleBorder(
        borderRadius: BorderRadius.circular(14),
        side: const BorderSide(color: line),
      ),
      child: ConstrainedBox(
        constraints: const BoxConstraints(maxWidth: 640, maxHeight: 560),
        child: DefaultTabController(
          length: 4,
          child: Column(
            children: [
              Padding(
                padding: const EdgeInsets.fromLTRB(22, 18, 14, 6),
                child: Row(
                  children: [
                    const _AppLogo(size: 32),
                    const SizedBox(width: 12),
                    const Column(
                      crossAxisAlignment: CrossAxisAlignment.start,
                      children: [
                        Text('鏡屏 MirrPane 設定',
                            style: TextStyle(
                                fontSize: 17, fontWeight: FontWeight.w500)),
                        Text('v0.3.1 · AirPlay Screen Receiver',
                            style: TextStyle(fontSize: 11.5, color: muted)),
                      ],
                    ),
                    const Spacer(),
                    IconButton(
                      onPressed:
                          _busy ? null : () => Navigator.of(context).pop(),
                      icon: const Icon(Icons.close, size: 18),
                    ),
                  ],
                ),
              ),
              const TabBar(
                isScrollable: true,
                tabAlignment: TabAlignment.start,
                indicatorColor: accent,
                labelColor: accent,
                unselectedLabelColor: muted,
                dividerColor: line,
                tabs: [
                  Tab(text: '一般'),
                  Tab(text: '圖形與解碼'),
                  Tab(text: '畫面與色彩'),
                  Tab(text: '音訊'),
                ],
              ),
              Expanded(
                child: TabBarView(
                  children: [
                    _buildGeneralTab(),
                    _buildGraphicsTab(),
                    _buildRenderTab(),
                    _buildAudioTab(),
                  ],
                ),
              ),
              Container(
                padding:
                    const EdgeInsets.symmetric(horizontal: 22, vertical: 14),
                decoration: const BoxDecoration(
                  border: Border(top: BorderSide(color: line)),
                ),
                child: Row(
                  children: [
                    if (_backendDirty)
                      const Row(
                        children: [
                          Icon(Icons.info_outline, size: 14, color: accent),
                          SizedBox(width: 6),
                          Text('部分變更需「儲存並重啟」生效',
                              style: TextStyle(fontSize: 12, color: accent)),
                        ],
                      ),
                    const Spacer(),
                    TextButton(
                      onPressed:
                          _busy ? null : () => Navigator.of(context).pop(),
                      child: const Text('關閉'),
                    ),
                    const SizedBox(width: 8),
                    FilledButton(
                      onPressed:
                          _busy ? null : () => unawaited(_saveAndRestart()),
                      style: FilledButton.styleFrom(
                        backgroundColor: _backendDirty ? accent : surfaceHi,
                        foregroundColor: _backendDirty ? ground : ink,
                      ),
                      child: Text(_busy ? '套用中…' : '儲存並重啟'),
                    ),
                  ],
                ),
              ),
            ],
          ),
        ),
      ),
    );
  }

  Widget _buildGeneralTab() {
    final s = widget.state;
    return ListView(
      padding: const EdgeInsets.all(22),
      children: [
        const Text('顯示名稱（廣播於 AirPlay 清單）',
            style: TextStyle(fontSize: 12.5, color: muted)),
        const SizedBox(height: 8),
        TextField(
          controller: _name,
          enabled: !_busy,
          onChanged: (_) => setState(() => _backendDirty = true),
          decoration: InputDecoration(
            filled: true,
            fillColor: ground,
            border: OutlineInputBorder(
              borderRadius: BorderRadius.circular(8),
              borderSide: const BorderSide(color: line),
            ),
            contentPadding:
                const EdgeInsets.symmetric(horizontal: 12, vertical: 10),
          ),
        ),
        const SizedBox(height: 18),
        if (s != null) ...[
          _InfoRow('區域網路 IP', s.ip.isEmpty ? '—' : '${s.ip}:${s.port}'),
          _InfoRow('當前解碼引擎', s.decoder.isEmpty ? '待機' : s.decoder),
          _InfoRow('累計畫面', '${s.pictures} 幀（略過 ${s.dropped} 幀）'),
          _InfoRow('音訊封包', '${s.audioFrames} 個'),
          _InfoRow('協定版本', 'AirPlay ${s.version}'),
          const Divider(color: line, height: 28),
        ],
        Wrap(
          spacing: 10,
          runSpacing: 10,
          children: [
            OutlinedButton.icon(
              onPressed: widget.onOpenScreenshots,
              style: OutlinedButton.styleFrom(
                  foregroundColor: ink, side: const BorderSide(color: line)),
              icon: const Icon(Icons.photo_library_outlined, size: 16),
              label: const Text('開啟截圖資料夾'),
            ),
            OutlinedButton.icon(
              onPressed: () => unawaited(
                Process.run('explorer.exe', [widget.host.installDir.path]),
              ),
              style: OutlinedButton.styleFrom(
                  foregroundColor: ink, side: const BorderSide(color: line)),
              icon: const Icon(Icons.folder_open, size: 16),
              label: const Text('開啟程式資料夾'),
            ),
            TextButton.icon(
              onPressed: _busy ? null : () => unawaited(_resetIdentity()),
              style: TextButton.styleFrom(foregroundColor: danger),
              icon: const Icon(Icons.restart_alt, size: 16),
              label: const Text('重設配對身分'),
            ),
          ],
        ),
      ],
    );
  }

  Widget _buildGraphicsTab() {
    return ListView(
      padding: const EdgeInsets.all(22),
      children: [
        const _SectionNote('以下項目寫入 config.json，需點擊「儲存並重啟」後生效'),
        const SizedBox(height: 14),
        _DropdownRow<String>(
          label: '虛擬顯示解析度',
          subtitle: '向 iPhone / iPad 宣告的建議畫質上限',
          value: _resKey,
          items: _resolutions.entries
              .map((e) => DropdownMenuItem(value: e.key, child: Text(e.value.$3)))
              .toList(),
          onChanged: (v) {
            if (v != null) {
              setState(() {
                _resKey = v;
                _backendDirty = true;
              });
            }
          },
        ),
        const SizedBox(height: 12),
        _DropdownRow<int>(
          label: '宣告幀率上限',
          subtitle: 'AirPlay 鏡像協定上限為 60 fps；30 fps 可降低頻寬消耗',
          value: _maxFps,
          items: const [
            DropdownMenuItem(value: 60, child: Text('60 fps（流暢 · 預設）')),
            DropdownMenuItem(value: 30, child: Text('30 fps（省流模式）')),
          ],
          onChanged: (v) {
            if (v != null) {
              setState(() {
                _maxFps = v;
                _backendDirty = true;
              });
            }
          },
        ),
        const SizedBox(height: 12),
        _DropdownRow<String>(
          label: 'H.264 解碼模式',
          subtitle: '自動模式優先使用 D3D11VA 硬體加速，失敗時退回 CPU',
          value: _decoderMode,
          items: const [
            DropdownMenuItem(value: 'auto', child: Text('自動（優先 D3D11VA 硬解）')),
            DropdownMenuItem(value: 'software', child: Text('強制軟體解碼 (CPU)')),
          ],
          onChanged: (v) {
            if (v != null) {
              setState(() {
                _decoderMode = v;
                _backendDirty = true;
              });
            }
          },
        ),
        const SizedBox(height: 12),
        _DropdownRow<String>(
          label: '輸入色彩範圍',
          subtitle: '若特定遊戲／影片顯得發灰或過暗，可手動切換 Full / Limited',
          value: _colorRange,
          items: const [
            DropdownMenuItem(value: 'auto', child: Text('自動偵測（推薦）')),
            DropdownMenuItem(value: 'full', child: Text('Full Range (0–255)')),
            DropdownMenuItem(value: 'limited', child: Text('Limited / TV (16–235)')),
          ],
          onChanged: (v) {
            if (v != null) {
              setState(() {
                _colorRange = v;
                _backendDirty = true;
              });
            }
          },
        ),
        const SizedBox(height: 12),
        _DropdownRow<String>(
          label: 'YUV → RGBA 色彩轉換演算法',
          subtitle: '控制 FFmpeg swscale 的色彩內插品質',
          value: _scaleFilter,
          items: const [
            DropdownMenuItem(value: 'fast', child: Text('速度優先 (Fast Bilinear · 預設)')),
            DropdownMenuItem(value: 'bilinear', child: Text('平衡 (Bilinear)')),
            DropdownMenuItem(value: 'bicubic', child: Text('銳利 (Bicubic)')),
          ],
          onChanged: (v) {
            if (v != null) {
              setState(() {
                _scaleFilter = v;
                _backendDirty = true;
              });
            }
          },
        ),
      ],
    );
  }

  Widget _buildRenderTab() {
    return ListView(
      padding: const EdgeInsets.all(22),
      children: [
        const _SectionNote('以下項目由 GPU 即時處理並自動儲存，免重啟立即生效'),
        const SizedBox(height: 14),
        _DropdownRow<String>(
          label: '視窗縮放濾鏡 (FilterQuality)',
          subtitle: '控制非 1:1 視窗大小時的貼圖取樣銳利度',
          value: _prefs.filterQuality,
          items: const [
            DropdownMenuItem(value: 'none', child: Text('無（最近鄰 · 像素銳利）')),
            DropdownMenuItem(value: 'low', child: Text('低（快速雙線性）')),
            DropdownMenuItem(value: 'medium', child: Text('中（平滑雙線性 · 預設）')),
            DropdownMenuItem(value: 'high', child: Text('高（Mipmap 抗鋸齒）')),
          ],
          onChanged: (v) {
            if (v != null) _setPrefs(_prefs.copyWith(filterQuality: v));
          },
        ),
        const SizedBox(height: 8),
        SwitchListTile(
          contentPadding: EdgeInsets.zero,
          title: const Text('水平鏡像翻轉 (H)',
              style: TextStyle(fontSize: 13.5, color: ink)),
          subtitle: const Text('適合前置鏡頭或自拍直播展示',
              style: TextStyle(fontSize: 12, color: muted)),
          value: _prefs.flipHorizontal,
          activeThumbColor: accent,
          onChanged: (v) => _setPrefs(_prefs.copyWith(flipHorizontal: v)),
        ),
        SwitchListTile(
          contentPadding: EdgeInsets.zero,
          title: const Text('填滿視窗裁切顯示 (Z)',
              style: TextStyle(fontSize: 13.5, color: ink)),
          subtitle: const Text('關閉時維持原始長寬比（含黑邊）',
              style: TextStyle(fontSize: 12, color: muted)),
          value: _prefs.fillViewport,
          activeThumbColor: accent,
          onChanged: (v) => _setPrefs(_prefs.copyWith(fillViewport: v)),
        ),
        const Divider(color: line, height: 22),
        Row(
          children: [
            const Text('即時色彩微調',
                style: TextStyle(fontSize: 13, fontWeight: FontWeight.w500)),
            const Spacer(),
            TextButton(
              onPressed: _prefs.isDefaultColor
                  ? null
                  : () => _setPrefs(_prefs.copyWith(
                        brightness: 0.0,
                        contrast: 1.0,
                        saturation: 1.0,
                      )),
              child: const Text('還原預設'),
            ),
          ],
        ),
        _SliderRow(
          label: '亮度',
          value: _prefs.brightness,
          min: -0.3,
          max: 0.3,
          display:
              '${_prefs.brightness >= 0 ? '+' : ''}${(_prefs.brightness * 100).round()}%',
          onChanged: (v) => _setPrefs(_prefs.copyWith(brightness: v)),
        ),
        _SliderRow(
          label: '對比度',
          value: _prefs.contrast,
          min: 0.7,
          max: 1.4,
          display: '${_prefs.contrast.toStringAsFixed(2)}×',
          onChanged: (v) => _setPrefs(_prefs.copyWith(contrast: v)),
        ),
        _SliderRow(
          label: '飽和度',
          value: _prefs.saturation,
          min: 0.5,
          max: 1.5,
          display: '${(_prefs.saturation * 100).round()}%',
          onChanged: (v) => _setPrefs(_prefs.copyWith(saturation: v)),
        ),
        const Divider(color: line, height: 22),
        SwitchListTile(
          contentPadding: EdgeInsets.zero,
          title: const Text('常駐效能監控 HUD (Tab)',
              style: TextStyle(fontSize: 13.5, color: ink)),
          value: _prefs.showHud,
          activeThumbColor: accent,
          onChanged: (v) => _setPrefs(_prefs.copyWith(showHud: v)),
        ),
        _DropdownRow<String>(
          label: 'HUD 顯示位置',
          value: _prefs.hudCorner,
          items: const [
            DropdownMenuItem(value: 'bottomLeft', child: Text('左下角（預設）')),
            DropdownMenuItem(value: 'bottomRight', child: Text('右下角')),
            DropdownMenuItem(value: 'topLeft', child: Text('左上角')),
            DropdownMenuItem(value: 'topRight', child: Text('右上角')),
          ],
          onChanged: (v) {
            if (v != null) _setPrefs(_prefs.copyWith(hudCorner: v));
          },
        ),
        const SizedBox(height: 12),
        _DropdownRow<int>(
          label: '頂列工具條自動隱藏',
          value: _prefs.barHideSeconds,
          items: const [
            DropdownMenuItem(value: 2, child: Text('閒置 2 秒後隱藏（預設）')),
            DropdownMenuItem(value: 4, child: Text('閒置 4 秒後隱藏')),
            DropdownMenuItem(value: 0, child: Text('永遠常駐顯示')),
          ],
          onChanged: (v) {
            if (v != null) _setPrefs(_prefs.copyWith(barHideSeconds: v));
          },
        ),
      ],
    );
  }

  Widget _buildAudioTab() {
    return ListView(
      padding: const EdgeInsets.all(22),
      children: [
        const Text('即時輸出控制（立即生效並記憶）',
            style: TextStyle(fontSize: 12.5, color: muted)),
        const SizedBox(height: 10),
        _SliderRow(
          label: '音量',
          value: _prefs.muted ? 0.0 : _prefs.volume,
          min: 0.0,
          max: 1.0,
          display: _prefs.muted ? '靜音' : '${(_prefs.volume * 100).round()}%',
          onChanged: (v) {
            _setPrefs(_prefs.copyWith(volume: v, muted: v <= 0.001));
            widget.onVolumeChanged(v);
          },
        ),
        SwitchListTile(
          contentPadding: EdgeInsets.zero,
          title: const Text('靜音輸出 (M)',
              style: TextStyle(fontSize: 13.5, color: ink)),
          value: _prefs.muted,
          activeThumbColor: accent,
          onChanged: (v) {
            _setPrefs(_prefs.copyWith(muted: v));
            widget.onMuteChanged(v);
          },
        ),
        const Divider(color: line, height: 28),
        const _SectionNote('以下項目寫入 config.json，需「儲存並重啟」後生效'),
        const SizedBox(height: 14),
        _DropdownRow<String>(
          label: '音訊抗抖動緩衝檔位',
          subtitle: 'Wi-Fi 環境良好可選「超低延遲」；若偶有爆音或斷續請選「弱網穩定」',
          value: _audioBuffer,
          items: const [
            DropdownMenuItem(value: 'low', child: Text('超低延遲（3 × 10 ms / 上限 80 ms）')),
            DropdownMenuItem(value: 'balanced', child: Text('標準平衡（4 × 10 ms / 上限 100 ms · 預設）')),
            DropdownMenuItem(value: 'stable', child: Text('弱網穩定（6 × 10 ms / 上限 150 ms）')),
          ],
          onChanged: (v) {
            if (v != null) {
              setState(() {
                _audioBuffer = v;
                _backendDirty = true;
              });
            }
          },
        ),
      ],
    );
  }
}

class _SectionNote extends StatelessWidget {
  const _SectionNote(this.text);

  final String text;

  @override
  Widget build(BuildContext context) {
    return Container(
      padding: const EdgeInsets.symmetric(horizontal: 12, vertical: 8),
      decoration: BoxDecoration(
        color: ground,
        borderRadius: BorderRadius.circular(8),
        border: Border.all(color: line),
      ),
      child: Row(
        children: [
          const Icon(Icons.tune, size: 15, color: accent),
          const SizedBox(width: 8),
          Expanded(
            child: Text(text, style: const TextStyle(fontSize: 12, color: muted)),
          ),
        ],
      ),
    );
  }
}

class _DropdownRow<T> extends StatelessWidget {
  const _DropdownRow({
    required this.label,
    this.subtitle,
    required this.value,
    required this.items,
    required this.onChanged,
  });

  final String label;
  final String? subtitle;
  final T value;
  final List<DropdownMenuItem<T>> items;
  final ValueChanged<T?> onChanged;

  @override
  Widget build(BuildContext context) {
    return Column(
      crossAxisAlignment: CrossAxisAlignment.start,
      children: [
        Text(label, style: const TextStyle(fontSize: 13.5, color: ink)),
        if (subtitle != null) ...[
          const SizedBox(height: 2),
          Text(subtitle!, style: const TextStyle(fontSize: 11.5, color: muted)),
        ],
        const SizedBox(height: 8),
        DropdownButtonFormField<T>(
          initialValue: value,
          items: items,
          onChanged: onChanged,
          dropdownColor: surfaceHi,
          style: const TextStyle(fontSize: 13, color: ink),
          decoration: InputDecoration(
            filled: true,
            fillColor: ground,
            isDense: true,
            contentPadding:
                const EdgeInsets.symmetric(horizontal: 12, vertical: 10),
            border: OutlineInputBorder(
              borderRadius: BorderRadius.circular(8),
              borderSide: const BorderSide(color: line),
            ),
            enabledBorder: OutlineInputBorder(
              borderRadius: BorderRadius.circular(8),
              borderSide: const BorderSide(color: line),
            ),
          ),
        ),
      ],
    );
  }
}

class _SliderRow extends StatelessWidget {
  const _SliderRow({
    required this.label,
    required this.value,
    required this.min,
    required this.max,
    required this.display,
    required this.onChanged,
  });

  final String label;
  final double value;
  final double min;
  final double max;
  final String display;
  final ValueChanged<double> onChanged;

  @override
  Widget build(BuildContext context) {
    return Row(
      children: [
        SizedBox(
          width: 56,
          child: Text(label, style: const TextStyle(fontSize: 12.5, color: muted)),
        ),
        Expanded(
          child: Slider(
            value: value.clamp(min, max),
            min: min,
            max: max,
            activeColor: accent,
            onChanged: onChanged,
          ),
        ),
        SizedBox(
          width: 52,
          child: Text(
            display,
            textAlign: TextAlign.right,
            style: mono.copyWith(fontSize: 12, color: ink),
          ),
        ),
      ],
    );
  }
}

class _InfoRow extends StatelessWidget {
  const _InfoRow(this.label, this.value);

  final String label;
  final String value;

  @override
  Widget build(BuildContext context) {
    return Padding(
      padding: const EdgeInsets.symmetric(vertical: 3.5),
      child: Row(
        children: [
          Text(label, style: const TextStyle(fontSize: 12.5, color: muted)),
          const Spacer(),
          Text(value, style: mono.copyWith(fontSize: 12.5, color: ink)),
        ],
      ),
    );
  }
}

class _ErrorView extends StatefulWidget {
  const _ErrorView({
    super.key,
    required this.message,
    required this.host,
    required this.onRetry,
  });

  final String message;
  final ReceiverHost host;
  final Future<void> Function() onRetry;

  @override
  State<_ErrorView> createState() => _ErrorViewState();
}

class _ErrorViewState extends State<_ErrorView> {
  List<String> _tail = const [];
  bool _retrying = false;

  @override
  void initState() {
    super.initState();
    unawaited(_loadTail());
  }

  Future<void> _loadTail() async {
    final tail = await widget.host.logTail();
    if (mounted) setState(() => _tail = tail);
  }

  Future<void> _retry() async {
    setState(() => _retrying = true);
    await widget.onRetry();
    if (mounted) setState(() => _retrying = false);
  }

  @override
  Widget build(BuildContext context) {
    return Center(
      child: ConstrainedBox(
        constraints: const BoxConstraints(maxWidth: 720),
        child: Padding(
          padding: const EdgeInsets.all(40),
          child: Column(
            mainAxisSize: MainAxisSize.min,
            crossAxisAlignment: CrossAxisAlignment.start,
            children: [
              const Row(
                children: [
                  Icon(Icons.error_outline, color: danger, size: 20),
                  SizedBox(width: 10),
                  Text('接收器無法啟動',
                      style:
                          TextStyle(fontSize: 28, fontWeight: FontWeight.w500)),
                ],
              ),
              const SizedBox(height: 10),
              Text(widget.message,
                  style: const TextStyle(fontSize: 16, color: ink)),
              if (_tail.isNotEmpty) ...[
                const SizedBox(height: 20),
                Container(
                  width: double.infinity,
                  padding: const EdgeInsets.all(16),
                  decoration: BoxDecoration(
                    color: surface,
                    borderRadius: BorderRadius.circular(6),
                    border: Border.all(color: line),
                  ),
                  child: Text(
                    _tail.join('\n'),
                    style:
                        mono.copyWith(fontSize: 12, height: 1.5, color: muted),
                  ),
                ),
              ],
              const SizedBox(height: 24),
              FilledButton(
                onPressed: _retrying ? null : _retry,
                style: FilledButton.styleFrom(
                  backgroundColor: accent,
                  foregroundColor: ground,
                ),
                child: Text(_retrying ? '重新啟動中…' : '重新偵測'),
              ),
            ],
          ),
        ),
      ),
    );
  }
}
