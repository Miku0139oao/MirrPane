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
    required this.fps,
    required this.decodeAvgMs,
    required this.decodeP95Ms,
    required this.downloadAvgMs,
    required this.downloadP95Ms,
    required this.convertAvgMs,
    required this.convertP95Ms,
    required this.publishAvgMs,
    required this.publishP95Ms,
    required this.lagFrames,
    required this.consumerAgeMs,
    required this.consumerAdopted,
    required this.hwMode,
    required this.audioOccupancyMs,
    required this.audioUnderruns,
    required this.audioHighWaterMs,
    required this.pendingSender,
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

  // Pipeline telemetry, additive to the sidecar's /api/state body. All
  // durations are milliseconds and every value falls back to zero when an
  // older sidecar does not send the key.
  final double fps;
  final double decodeAvgMs;
  final double decodeP95Ms;
  final double downloadAvgMs;
  final double downloadP95Ms;
  final double convertAvgMs;
  final double convertP95Ms;
  final double publishAvgMs;
  final double publishP95Ms;
  final int lagFrames;
  final double consumerAgeMs;
  final int consumerAdopted;
  final String hwMode; // D3D11VA | software | ""
  final double audioOccupancyMs;
  final int audioUnderruns;
  final double audioHighWaterMs;

  /// A verified sender waiting for the local user's approval, or null.
  final PendingSender? pendingSender;

  static ReceiverState fromJson(Map<String, dynamic> json) {
    int asInt(Object? v, [int fallback = 0]) => v is num ? v.toInt() : fallback;
    double asDouble(Object? v, [double fallback = 0]) =>
        v is num ? v.toDouble() : fallback;
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
      fps: asDouble(json['fps']),
      decodeAvgMs: asDouble(json['decodeAvgMs']),
      decodeP95Ms: asDouble(json['decodeP95Ms']),
      downloadAvgMs: asDouble(json['downloadAvgMs']),
      downloadP95Ms: asDouble(json['downloadP95Ms']),
      convertAvgMs: asDouble(json['convertAvgMs']),
      convertP95Ms: asDouble(json['convertP95Ms']),
      publishAvgMs: asDouble(json['publishAvgMs']),
      publishP95Ms: asDouble(json['publishP95Ms']),
      lagFrames: asInt(json['lagFrames']),
      consumerAgeMs: asDouble(json['consumerAgeMs']),
      consumerAdopted: asInt(json['consumerAdopted']),
      hwMode: asString(json['hwMode']),
      audioOccupancyMs: asDouble(json['audioOccupancyMs']),
      audioUnderruns: asInt(json['audioUnderruns']),
      audioHighWaterMs: asDouble(json['audioHighWaterMs']),
      pendingSender: PendingSender.fromJson(json['pendingSender']),
    );
  }
}

/// One verified sender awaiting owner approval before it may stream.
class PendingSender {
  const PendingSender({
    required this.fingerprint,
    required this.remote,
    required this.ageSeconds,
  });

  final String fingerprint;
  final String remote;
  final int ageSeconds;

  static PendingSender? fromJson(Object? value) {
    if (value is! Map) return null;
    final fingerprint = value['fingerprint'];
    if (fingerprint is! String || fingerprint.isEmpty) return null;
    final remote = value['remote'];
    final age = value['ageSeconds'];
    return PendingSender(
      fingerprint: fingerprint,
      remote: remote is String && remote.isNotEmpty ? remote : '未知裝置',
      ageSeconds: age is num ? age.toInt() : 0,
    );
  }
}

/// What the UI renders. Everything is derived from the sidecar's own state, so
/// the window never claims to be mirroring when it is not.
enum Phase { booting, waiting, connecting, live, interrupted, failed }

class Snapshot {
  const Snapshot(this.phase, {this.state, this.error, this.message});
  final Phase phase;
  final ReceiverState? state;
  final String? error;
  final String? message;
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
  bool _runtimeSetup = false;
  String? _startupError;

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
          '-api-port',
          '$_port',
          '-data',
          '${installDir.path}\\config',
          '-log',
          logPath,
          '-parent-pid',
          '$pid',
        ],
        workingDirectory: installDir.path,
        runInShell: false,
      );
    } on ProcessException catch (e) {
      _emitFailure('無法啟動接收器：${e.message}');
      return;
    }

    _process!.stdout
        .transform(utf8.decoder)
        .transform(const LineSplitter())
        .listen(_startupProgress, onError: (Object _) {});
    unawaited(_process!.stderr.drain<void>());

    unawaited(
      _process!.exitCode.then((code) {
        if (_stopped || _everReached) return;
        _emitFailure(_startupError ?? '接收器結束了（代碼 $code）');
      }),
    );

    _http = HttpClient()..connectionTimeout = const Duration(seconds: 2);
    _poll = Timer.periodic(const Duration(milliseconds: 350), (_) => _tick());
    unawaited(_tick());
  }

  void _startupProgress(String line) {
    if (_stopped || _everReached) return;
    try {
      final event = jsonDecode(line);
      if (event is! Map<String, dynamic> ||
          event['event'] != 'ffmpeg_setup' ||
          event['message'] is! String) {
        return;
      }
      final message = event['message'] as String;
      _runtimeSetup = event['done'] != true;
      if (event['failed'] == true) {
        _startupError = message;
        _emitFailure(message);
        return;
      }
      if (!_runtimeSetup) _startedAt = DateTime.now();
      notifier.value = Snapshot(Phase.booting, message: message);
    } on FormatException {
      // Other diagnostic lines do not belong to the startup protocol.
    }
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

  /// Clears every trusted sender so the next verified device must be approved
  /// again through the local UI.
  Future<bool> resetTrustedSenders() async {
    final res = await _postJson('/api/pairing/trust/reset');
    unawaited(_tick());
    return res?['ok'] == true;
  }

  /// Approves a pending sender so its next setup may stream.
  Future<bool> approveSender(String fingerprint) async {
    final res = await _postJson('/api/pairing/trust/approve', {
      'fingerprint': fingerprint,
    });
    unawaited(_tick());
    return res?['ok'] == true;
  }

  /// Rejects a pending sender.
  Future<bool> rejectSender(String fingerprint) async {
    final res = await _postJson('/api/pairing/trust/reject', {
      'fingerprint': fingerprint,
    });
    unawaited(_tick());
    return res?['ok'] == true;
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
      final response = await request.close().timeout(
        const Duration(milliseconds: 700),
      );
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
      final deadline = _runtimeSetup
          ? const Duration(minutes: 11)
          : const Duration(seconds: 8);
      if (!_everReached && DateTime.now().difference(_startedAt!) > deadline) {
        _emitFailure(_startupError ?? '接收器沒有回應');
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
