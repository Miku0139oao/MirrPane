import 'dart:convert';
import 'dart:io';

import 'package:flutter_test/flutter_test.dart';
import 'package:yingping/receiver_api.dart';

/// P3.10 (and the native P2.16 deadlines the retained UI mirrors): a receiver
/// that answered once must not keep showing a stale picture when it exits or
/// stops answering, while a slow FFmpeg download that keeps reporting
/// progress must not be called dead.
void main() {
  test(
    'another receiver protects its config from settings and reset',
    () async {
      final root = await Directory.systemTemp.createTemp('MirrPane-settings-');
      addTearDown(() => root.delete(recursive: true));
      final host = ReceiverHost(root);
      expect(await host.saveName('Original'), isTrue);
      final config = File(host.configPath);
      final lease = await File.fromUri(
        config.parent.uri.resolve('receiver.lock'),
      ).open(mode: FileMode.append);
      try {
        await lease.lock(FileLock.exclusive, 0, 1);
        expect(await host.resetIdentity(), isFalse);
        expect(await host.saveName('Other'), isFalse);
        expect(jsonDecode(await config.readAsString())['name'], 'Original');
      } finally {
        await lease.close();
      }
      expect(await host.resetIdentity(), isTrue);
      expect(await config.exists(), isFalse);
    },
    skip: !Platform.isWindows,
  );

  late DateTime now;
  late ReceiverLifecycle life;

  setUp(() {
    now = DateTime(2026, 11, 26, 12);
    life = ReceiverLifecycle(clock: () => now);
    life.start();
  });

  String setupLine(String message, {bool done = false, bool failed = false}) =>
      jsonEncode(<String, dynamic>{
        'event': 'ffmpeg_setup',
        'message': message,
        'done': done,
        'failed': failed,
      });

  test('a progressing download is never reported dead by elapsed time', () {
    now = now.add(const Duration(minutes: 12));
    final progress = life.onStartupLine(setupLine('下載 FFmpeg 60%'));
    expect(progress, isNotNull);
    expect(progress!.failed, isFalse);
    expect(life.onPollError(), isNull);

    // Twelve minutes later with recent progress is fine; without progress it
    // trips the idle deadline once.
    now = now.add(const Duration(minutes: 10));
    expect(life.onPollError(), isNull);
    now = now.add(const Duration(minutes: 2));
    expect(life.onPollError(), '接收器沒有回應');
    expect(life.onPollError(), isNull, reason: 'one failure per outage');
  });

  test('the whole provisioning step has the backend 30-minute cap', () {
    now = now.add(const Duration(minutes: 31));
    life.onStartupLine(setupLine('下載 FFmpeg 99%'));
    expect(life.onPollError(), '接收器啟動逾時（30 分鐘）');
    expect(ReceiverLifecycle.setupTotalTimeout, const Duration(minutes: 30));
  });

  test('an exit after the first state is reported, not hidden', () {
    expect(life.onState(), isTrue);
    expect(life.onExit(7), '接收器意外結束（代碼 7）');
    expect(life.dead, isTrue);
    expect(life.onExit(7), isNull, reason: 'the exit is reported once');
    expect(life.onState(), isFalse, reason: 'a dead launch takes no state');
    expect(life.onPollError(), isNull);
  });

  test('a running receiver that stops answering is reported and recovers', () {
    life.onState();
    now = now.add(const Duration(seconds: 5));
    expect(life.onPollError(), isNull);
    now = now.add(const Duration(seconds: 4));
    expect(life.onPollError(), '接收器已停止回應，正在重新連線…');
    expect(life.onPollError(), isNull);

    now = now.add(const Duration(seconds: 1));
    expect(life.onState(), isTrue);
    now = now.add(const Duration(seconds: 9));
    expect(
      life.onPollError(),
      '接收器已停止回應，正在重新連線…',
      reason: 'a recovered receiver reports a later outage afresh',
    );
  });

  test('a startup failure names itself and is not overwritten by the exit', () {
    final failure = life.onStartupLine(
      setupLine('此資料夾正由另一個 MirrPane 接收器使用', done: true, failed: true),
    );
    expect(failure, isNotNull);
    expect(failure!.failed, isTrue);
    expect(failure.message, '此資料夾正由另一個 MirrPane 接收器使用');

    // The process exits after reporting; the failure stays as it is.
    expect(life.onExit(1), isNull);
    // Neither does a stray progress line flip the screen back to booting.
    expect(life.onStartupLine(setupLine('清理中')), isNull);
  });

  test('a silent startup failure still has a message', () {
    final failure = life.onStartupLine(
      jsonEncode(<String, dynamic>{'event': 'ffmpeg_setup', 'failed': true}),
    );
    expect(failure?.failed, isTrue);
    expect(failure?.message, '接收器無法啟動');
  });

  test('an exit before the first state says the receiver ended', () {
    expect(life.onExit(9), '接收器結束了（代碼 9）');
  });

  test('lines outside the startup protocol are ignored', () {
    expect(life.onStartupLine('not json'), isNull);
    expect(
      life.onStartupLine(jsonEncode({'event': 'api_ready', 'port': 1})),
      isNull,
    );
    expect(life.onStartupLine(setupLine('')), isNull);
  });

  test('a deliberate stop suppresses every later report', () {
    life.onState();
    life.stop();
    expect(life.onPollError(), isNull);
    expect(life.onExit(0), isNull);
  });

  test('audio-only live state keeps its sender and reason', () {
    final state = ReceiverState.fromJson(<String, dynamic>{
      'status': 'live',
      'reason': 'audio',
      'sender': 'iPhone 客廳',
    });
    expect(state.status, 'live');
    expect(state.reason, 'audio');
    expect(state.sender, 'iPhone 客廳');
  });
}
