import 'dart:convert';
import 'dart:io';

import 'package:flutter/material.dart';

/// Persistent frontend preferences stored beside config.json in config/ui_prefs.json.
/// Everything here takes effect immediately on the GPU/UI without restarting airplayd.
class UiPrefs {
  const UiPrefs({
    this.filterQuality = 'medium',
    this.brightness = 0.0,
    this.contrast = 1.0,
    this.saturation = 1.0,
    this.flipHorizontal = false,
    this.fillViewport = false,
    this.showHud = false,
    this.hudCorner = 'bottomLeft',
    this.barHideSeconds = 2,
    this.muted = false,
    this.volume = 1.0,
  });

  final String filterQuality; // none | low | medium | high
  final double brightness; // -0.3 .. 0.3
  final double contrast; // 0.7 .. 1.4
  final double saturation; // 0.5 .. 1.5
  final bool flipHorizontal;
  final bool fillViewport;
  final bool showHud;
  final String hudCorner; // topLeft | topRight | bottomLeft | bottomRight
  final int barHideSeconds; // 0 | 2 | 4
  final bool muted;
  final double volume; // 0.0 .. 1.0

  UiPrefs copyWith({
    String? filterQuality,
    double? brightness,
    double? contrast,
    double? saturation,
    bool? flipHorizontal,
    bool? fillViewport,
    bool? showHud,
    String? hudCorner,
    int? barHideSeconds,
    bool? muted,
    double? volume,
  }) {
    return UiPrefs(
      filterQuality: filterQuality ?? this.filterQuality,
      brightness: (brightness ?? this.brightness).clamp(-0.3, 0.3),
      contrast: (contrast ?? this.contrast).clamp(0.7, 1.4),
      saturation: (saturation ?? this.saturation).clamp(0.5, 1.5),
      flipHorizontal: flipHorizontal ?? this.flipHorizontal,
      fillViewport: fillViewport ?? this.fillViewport,
      showHud: showHud ?? this.showHud,
      hudCorner: hudCorner ?? this.hudCorner,
      barHideSeconds: barHideSeconds ?? this.barHideSeconds,
      muted: muted ?? this.muted,
      volume: (volume ?? this.volume).clamp(0.0, 1.0),
    );
  }

  FilterQuality toFlutterFilterQuality() {
    switch (filterQuality) {
      case 'none':
        return FilterQuality.none;
      case 'low':
        return FilterQuality.low;
      case 'high':
        return FilterQuality.high;
      default:
        return FilterQuality.medium;
    }
  }

  bool get isDefaultColor =>
      brightness.abs() < 0.001 &&
      (contrast - 1.0).abs() < 0.001 &&
      (saturation - 1.0).abs() < 0.001;

  /// Builds a combined Rec.709 saturation, contrast, and brightness 4x5 matrix.
  /// Returns null when all color controls are at neutral defaults.
  ColorFilter? buildColorFilter() {
    if (isDefaultColor) return null;

    const lumR = 0.2126;
    const lumG = 0.7152;
    const lumB = 0.0722;
    final s = saturation;
    final sr = (1.0 - s) * lumR;
    final sg = (1.0 - s) * lumG;
    final sb = (1.0 - s) * lumB;

    final c = contrast;
    final offset = 128.0 * (1.0 - c) + brightness * 255.0;

    return ColorFilter.matrix(<double>[
      c * (sr + s), c * sg, c * sb, 0, offset,
      c * sr, c * (sg + s), c * sb, 0, offset,
      c * sr, c * sg, c * (sb + s), 0, offset,
      0, 0, 0, 1, 0,
    ]);
  }

  Map<String, dynamic> toJson() => {
        'filterQuality': filterQuality,
        'brightness': double.parse(brightness.toStringAsFixed(3)),
        'contrast': double.parse(contrast.toStringAsFixed(3)),
        'saturation': double.parse(saturation.toStringAsFixed(3)),
        'flipHorizontal': flipHorizontal,
        'fillViewport': fillViewport,
        'showHud': showHud,
        'hudCorner': hudCorner,
        'barHideSeconds': barHideSeconds,
        'muted': muted,
        'volume': double.parse(volume.toStringAsFixed(3)),
      };

  static UiPrefs fromJson(Map<String, dynamic> json) {
    String str(String k, List<String> allowed, String fallback) {
      final v = json[k];
      return (v is String && allowed.contains(v)) ? v : fallback;
    }

    double numIn(String k, double min, double max, double fallback) {
      final v = json[k];
      return v is num ? v.toDouble().clamp(min, max) : fallback;
    }

    final hide = json['barHideSeconds'];
    final validHide =
        (hide is num && (hide.toInt() == 0 || hide.toInt() == 2 || hide.toInt() == 4))
            ? hide.toInt()
            : 2;

    return UiPrefs(
      filterQuality:
          str('filterQuality', const ['none', 'low', 'medium', 'high'], 'medium'),
      brightness: numIn('brightness', -0.3, 0.3, 0.0),
      contrast: numIn('contrast', 0.7, 1.4, 1.0),
      saturation: numIn('saturation', 0.5, 1.5, 1.0),
      flipHorizontal: json['flipHorizontal'] == true,
      fillViewport: json['fillViewport'] == true,
      showHud: json['showHud'] == true,
      hudCorner: str(
        'hudCorner',
        const ['topLeft', 'topRight', 'bottomLeft', 'bottomRight'],
        'bottomLeft',
      ),
      barHideSeconds: validHide,
      muted: json['muted'] == true,
      volume: numIn('volume', 0.0, 1.0, 1.0),
    );
  }

  static String pathFor(String installPath) =>
      '$installPath\\config\\ui_prefs.json';

  static Future<UiPrefs> load(String installPath) async {
    try {
      final file = File(pathFor(installPath));
      if (!await file.exists()) return const UiPrefs();
      final decoded = jsonDecode(await file.readAsString());
      if (decoded is Map<String, dynamic>) {
        return fromJson(decoded);
      }
    } catch (_) {}
    return const UiPrefs();
  }

  Future<void> save(String installPath) async {
    try {
      final file = File(pathFor(installPath));
      await file.parent.create(recursive: true);
      await file.writeAsString(
        const JsonEncoder.withIndent('  ').convert(toJson()),
      );
    } catch (_) {}
  }
}
