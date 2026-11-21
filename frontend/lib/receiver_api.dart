import 'dart:async';
import 'dart:convert';
import 'dart:io';

import 'package:flutter/foundation.dart';

/// One state sample from the sidecar's localhost API.
class ReceiverState {
  const ReceiverState({
    required this.status,
    required this.reason,
    required this.name,
    required this.ip,
    required this.port,
    required this.sender,
    required this.width,
    required this.height,
    required this.videoFrames,
    required this.audioFrames,
    required this.pictures,
    required this.dropped,
    required this.decoder,
    required this.muted,
    required this.volume,
    required this.uptimeSeconds,
    required this.version,
    required this.targetWidth,
    required this.targetHeight,
    required this.maxFps,
    required this.decoderMode,
    required this.colorRange,
    required this.scaleFilter,
    required this.audioBuffer,
  });

  final String status; // idle | connecting | live | interrupted
  final String reason;
  final String name;
  final String ip;
  final int port;
  final String sender;
  final int width;
  final int height;
  final int videoFrames;
  final int audioFrames;
  final int pictures;
  final int dropped;
  final String decoder; // D3D11VA | software | ""
  final bool muted;
  final double volume;
  final double uptimeSeconds;
  final String version;
  final int targetWidth;
  final int targetHeight;
  final int maxFps;
  final String decoderMode;
  final String colorRange;
  final String scaleFilter;
  final String audioBuffer;

  static ReceiverState fromJson(Map<String, dynamic> json) {
    int asInt(Object? v, [int fallback = 0]) => v is num ? v.toInt() : fallback;
    String asString(Object? v, [String fallback = '']) =>
        (v is String && v.isNotEmpty) ? v : fallback;
    return ReceiverState(
      status: asString(json['status'], 'idle'),
      reason: asString(json['reason']),
      name: asString(json['name']),
      ip: asString(json['ip']),
      port: asInt(json['port']),
      sender: asString(json['sender']),
      width: asInt(json['width']),
      height: asInt(json['height']),
      videoFrames: asInt(json['videoFrames']),
      audioFrames: asInt(json['audioFrames']),
      pictures: asInt(json['pictures']),
      dropped: asInt(json['dropped']),
      decoder: asString(json['decoder']),
      muted: json['muted'] == true,
      volume: (json['volume'] as num?)?.toDouble() ?? 1.0,
      uptimeSeconds: (json['uptimeSeconds'] as num?)?.toDouble() ?? 0,
      version: asString(json['version']),
      targetWidth: asInt(json['targetWidth'], 1920),
      targetHeight: asInt(json['targetHeight'], 1080),
      maxFps: asInt(json['maxFps'], 60),
      decoderMode: asString(json['decoderMode'], 'auto'),
      colorRange: asString(json['colorRange'], 'auto'),
      scaleFilter: asString(json['scaleFilter'], 'fast'),
      audioBuffer: asString(json['audioBuffer'], 'balanced'),
    );
  }
}

/// What the UI renders. Everything is derived from the sidecar's own state, so
/// the window never claims to be mirroring when it is not.
enum Phase { booting, waiting, connecting, live, interrupted, failed }

class Snapshot {
  const Snapshot(this.phase, {this.state, this.error});
  final Phase phase;
  final ReceiverState? state;
  final String? error;
}

/// ReceiverHost owns the sidecar process and talks to its localhost API.
class ReceiverHost {
  ReceiverHost(this.installDir);

  /// The folder holding MirrPane.exe, backend/ and config/.
  final Directory installDir;

  final notifier = ValueNotifier<Snapshot>(const Snapshot(Phase.booting));

  Process? _process;
  HttpClient? _http;
  int _port = 0;
  Timer? _poll;
  DateTime? _startedAt;
  bool _stopped = false;
  bool _everReached = false;

  String get logPath => '${installDir.path}\\logs\\airplayd.log';
  String get configPath => '${installDir.path}\\config\\config.json';
  String get screenshotsDir => '${installDir.path}\\screenshots';

  /// Merges patch into config.json, creating the file if it does not exist yet.
  Future<bool> saveConfig(Map<String, dynamic> patch) async {
    try {
      final file = File(configPath);
      Map<String, dynamic> data = <String, dynamic>{};
      if (await file.exists()) {
        final decoded = jsonDecode(await file.readAsString());
        if (decoded is Map<String, dynamic>) {
          data = decoded;
        }
      } else {
        await file.parent.create(recursive: true);
      }
      data.addAll(patch);
      await file.writeAsString(
        const JsonEncoder.withIndent('  ').convert(data),
      );
      return true;
    } catch (_) {
      return false;
    }
  }

  /// Updates the advertised receiver name in config.json.
  Future<bool> saveName(String newName) async {
    final trimmed = newName.trim();
    if (trimmed.isEmpty) return false;
    return saveConfig({'name': trimmed});
  }

  /// Removes config.json so the sidecar generates a fresh AirPlay identity on
  /// next start.
  Future<void> resetIdentity() async {
    try {
      final file = File(configPath);
      if (await file.exists()) await file.delete();
    } catch (_) {}
  }

  Future<void> start() async {
    _startedAt = DateTime.now();

    final exe = File('${installDir.path}\\backend\\airplayd.exe');
    if (!await exe.exists()) {
      _emitFailure('找不到 backend\\airplayd.exe');
      return;
    }

    try {
      final probe = await ServerSocket.bind(InternetAddress.loopbackIPv4, 0);
      _port = probe.port;
      await probe.close();
    } on SocketException catch (e) {
      _emitFailure('無法取得本機埠：${e.message}');
      return;
    }

    try {
      _process = await Process.start(
        exe.path,
        [
          '-api-port', '$_port',
          '-data', '${installDir.path}\\config',
          '-log', logPath,
          '-parent-pid', '$pid',
        ],
        workingDirectory: installDir.path,
        runInShell: false,
      );
    } on ProcessException catch (e) {
      _emitFailure('無法啟動接收器：${e.message}');
      return;
    }

    unawaited(_process!.stdout.drain<void>());
    unawaited(_process!.stderr.drain<void>());

    unawaited(_process!.exitCode.then((code) {
      if (_stopped || _everReached) return;
      _emitFailure('接收器結束了（代碼 $code）');
    }));

    _http = HttpClient()..connectionTimeout = const Duration(seconds: 2);
    _poll = Timer.periodic(const Duration(milliseconds: 350), (_) => _tick());
    unawaited(_tick());
  }

  Future<Map<String, dynamic>?> _postJson(
    String path, [
    Map<String, dynamic>? body,
  ]) async {
    if (_stopped || _port == 0) return null;
    final client = _http;
    if (client == null) return null;
    try {
      final req = await client
          .postUrl(Uri.parse('http://127.0.0.1:$_port$path'))
          .timeout(const Duration(seconds: 2));
      if (body != null) {
        req.headers.contentType = ContentType.json;
        req.write(jsonEncode(body));
      }
      final resp = await req.close().timeout(const Duration(seconds: 2));
      final text = await resp
          .transform(utf8.decoder)
          .join()
          .timeout(const Duration(seconds: 2));
      if (resp.statusCode != 200) return null;
      final decoded = jsonDecode(text);
      return decoded is Map<String, dynamic> ? decoded : null;
    } catch (_) {
      return null;
    }
  }

  /// Drops the current mirroring connection and returns to Waiting.
  Future<void> disconnect() async {
    await _postJson('/api/disconnect');
    unawaited(_tick());
  }

  /// Updates audio mute and/or linear volume [0.0, 1.0].
  Future<void> setAudio({bool? isMuted, double? volume}) async {
    final payload = <String, dynamic>{};
    if (isMuted != null) payload['muted'] = isMuted;
    if (volume != null) payload['volume'] = volume;
    if (payload.isEmpty) return;
    await _postJson('/api/audio', payload);
    unawaited(_tick());
  }

  /// Captures the latest decoded video frame to a PNG file and returns its path.
  Future<String?> takeSnapshot() async {
    final res = await _postJson('/api/snapshot');
    final path = res?['path'];
    return path is String && path.isNotEmpty ? path : null;
  }

  Future<void> _tick() async {
    if (_stopped || _port == 0) return;
    final client = _http;
    if (client == null) return;

    try {
      final request = await client
          .getUrl(Uri.parse('http://127.0.0.1:$_port/api/state'))
          .timeout(const Duration(milliseconds: 700));
      final response =
          await request.close().timeout(const Duration(milliseconds: 700));
      if (response.statusCode != 200) return;
      final body = await response
          .transform(utf8.decoder)
          .join()
          .timeout(const Duration(milliseconds: 700));
      final json = jsonDecode(body);
      if (json is! Map<String, dynamic>) return;

      final state = ReceiverState.fromJson(json);
      _everReached = true;
      notifier.value = Snapshot(_phaseOf(state), state: state);
    } catch (_) {
      if (!_everReached &&
          DateTime.now().difference(_startedAt!) > const Duration(seconds: 8)) {
        _emitFailure('接收器沒有回應');
      }
    }
  }

  Phase _phaseOf(ReceiverState state) {
    switch (state.status) {
      case 'live':
        return Phase.live;
      case 'interrupted':
        return Phase.interrupted;
      case 'connecting':
        return Phase.connecting;
      default:
        return Phase.waiting;
    }
  }

  void _emitFailure(String message) {
    if (_stopped) return;
    notifier.value = Snapshot(Phase.failed, error: message);
  }

  /// The last lines of the sidecar's log, for the log viewer and error screen.
  Future<List<String>> logTail({int lines = 12}) async {
    try {
      final file = File(logPath);
      if (!await file.exists()) return const [];
      final text = await file.readAsString();
      final all = const LineSplitter().convert(text);
      return all.length <= lines ? all : all.sublist(all.length - lines);
    } catch (_) {
      return const [];
    }
  }

  Future<void> stop() async {
    if (_stopped) return;
    _stopped = true;
    _poll?.cancel();
    _http?.close(force: true);
    final process = _process;
    if (process != null) {
      process.kill();
      await process.exitCode.timeout(
        const Duration(seconds: 2),
        onTimeout: () => -1,
      );
    }
  }
}
