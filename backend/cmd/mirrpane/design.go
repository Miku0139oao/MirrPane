package main

import (
	"github.com/egoist/mygo/ui"
)

// MirrPane's desktop colour palette: four warm luminance tiers, one amber
// accent — the colours of the app's mark. Parameters name the role, not the
// shade: ground is the chassis, surface the panels, SurfaceHi the raised
// instruments.
var (
	colGround    = ui.Hex("#110F0E")
	colWell      = ui.Hex("#141210")
	colSurface   = ui.Hex("#1A1714")
	colSurfaceHi = ui.Hex("#231F1B")
	colSurfacePr = ui.Hex("#2F2A23")
	colBorder    = ui.RGBA(255, 244, 230, 0.095)
	colHairline  = ui.RGBA(255, 244, 230, 0.055)
	colRim       = ui.RGBA(255, 246, 232, 0.075)
	colInk       = ui.Hex("#F4EFE7")
	colMuted     = ui.Hex("#B0A79B")
	colFaint     = ui.Hex("#8C8479")
	colAccent    = ui.Hex("#E8A24A")
	colAccentHov = ui.Hex("#F2B663")
	colAccentPrs = ui.Hex("#C9862F")
	colOK        = ui.Hex("#72CE8A")
	colDanger    = ui.Hex("#E57365")
	colWarn      = ui.Hex("#E8AE58")
	colGlass     = ui.RGBA(19, 17, 15, 0.72)
	colOverlayBg = ui.RGBA(19, 17, 16, 0.88)
	colHudBg     = ui.RGBA(15, 13, 12, 0.76)
	colCardBg    = ui.RGBA(255, 244, 230, 0.045)
	colHover     = ui.RGBA(255, 244, 230, 0.075)
	colMenuBg    = ui.Hex("#231F1B")
)

func mirrpaneTheme() *ui.Theme {
	t := ui.DarkTheme()
	t.Background = colGround
	// Controls take the recessed well colour so they read as inset into the
	// panels they sit on, not painted with the same wall paint.
	t.Surface = colWell
	t.SurfaceHover = ui.Hex("#1F1B17")
	t.SurfacePressed = colSurfaceHi
	t.Border = colBorder
	t.Text = colInk
	t.TextMuted = colMuted
	t.Accent = colAccent
	t.AccentHover = colAccentHov
	t.AccentPressed = colAccentPrs
	t.AccentText = ui.Hex("#1B1208")
	t.Danger = colDanger
	t.Warning = colWarn
	t.Success = colOK
	t.Focus = ui.RGBA(232, 162, 74, 0.72)
	t.Radius = 9
	t.Spacing = 4
	t.Inverse = ui.Hex("#2A2521")
	t.InverseText = colInk
	t.Scrollbar = ui.RGBA(255, 244, 230, 0.2)
	t.ScrollbarStyle = ui.ScrollbarStyle{ThumbWidth: 5, Thumb: ui.RGBA(255, 244, 230, 0.22), Hover: ui.RGBA(255, 244, 230, 0.34)}
	return t
}

// svgIcon parses one of the app's line icons: 24×24, drawn in the text
// colour wherever it is shown.
func svgIcon(body string) *ui.SVG {
	return ui.MustParseSVG([]byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round">` + body + `</svg>`))
}

// icons are MirrPane's own line icons, parsed once at startup.
var icons = struct {
	volume, mute, camera, display, pin, fullscreen  *ui.SVG
	logs, settings, power, more, copy, folder       *ui.SVG
	refresh, rotate, flip, fill, hud, close         *ui.SVG
	check, chevron, alert, music, external, image   *ui.SVG
	phone, backArrow, homeIcon, recents, androidNav *ui.SVG
}{
	volume:     svgIcon(`<path d="M4 9.5h3.2L11.5 6v12L7.2 14.5H4z"/><path d="M15.2 9.2a4.2 4.2 0 0 1 0 5.6"/><path d="M17.8 6.6a8 8 0 0 1 0 10.8"/>`),
	mute:       svgIcon(`<path d="M4 9.5h3.2L11.5 6v12L7.2 14.5H4z"/><path d="M15.5 9.7l4.6 4.6M20.1 9.7l-4.6 4.6"/>`),
	camera:     svgIcon(`<path d="M8.4 7.2 9.6 5h4.8l1.2 2.2"/><rect x="3.5" y="7.2" width="17" height="12.3" rx="3"/><circle cx="12" cy="13.2" r="3.4"/>`),
	display:    svgIcon(`<rect x="3" y="4.5" width="18" height="12.5" rx="2.6"/><path d="M9 20.5h6M12 17v3.5"/>`),
	pin:        svgIcon(`<path d="M9.2 4h5.6l-.5 5.6 2.6 3.4H7.1l2.6-3.4L9.2 4z"/><path d="M12 13v7"/>`),
	fullscreen: svgIcon(`<path d="M4.5 9V4.5H9M15 4.5h4.5V9M19.5 15v4.5H15M9 19.5H4.5V15"/>`),
	logs:       svgIcon(`<path d="M4.5 6h15M4.5 10.5h15M4.5 15h9.5M4.5 19.5h6.5"/>`),
	settings:   svgIcon(`<circle cx="12" cy="12" r="3"/><circle cx="12" cy="12" r="6.6"/><path d="M12 2.4v2.4M12 19.2v2.4M2.4 12h2.4M19.2 12h2.4M5.2 5.2l1.7 1.7M17.1 17.1l1.7 1.7M18.8 5.2l-1.7 1.7M6.9 17.1l-1.7 1.7"/>`),
	power:      svgIcon(`<path d="M12 3.5v7.5"/><path d="M7 6.6a8 8 0 1 0 10 0"/>`),
	more:       svgIcon(`<circle cx="5.2" cy="12" r="1.5" fill="currentColor" stroke="none"/><circle cx="12" cy="12" r="1.5" fill="currentColor" stroke="none"/><circle cx="18.8" cy="12" r="1.5" fill="currentColor" stroke="none"/>`),
	copy:       svgIcon(`<rect x="9" y="9" width="11" height="11" rx="2.4"/><path d="M6 14.8H5.2A2.2 2.2 0 0 1 3 12.6V5.2A2.2 2.2 0 0 1 5.2 3h7.4A2.2 2.2 0 0 1 14.8 5.2V6"/>`),
	folder:     svgIcon(`<path d="M3.5 7.2A2.7 2.7 0 0 1 6.2 4.5h3.6l1.9 2.4h6.1a2.7 2.7 0 0 1 2.7 2.7v7.7a2.7 2.7 0 0 1-2.7 2.7H6.2a2.7 2.7 0 0 1-2.7-2.7V7.2z"/>`),
	refresh:    svgIcon(`<path d="M19.5 12a7.5 7.5 0 1 1-2.2-5.3"/><path d="M19.8 3.8v4.2h-4.2"/>`),
	rotate:     svgIcon(`<path d="M4.8 8.6A8 8 0 1 1 4.3 15"/><path d="M4.8 3.8v4.8h4.8"/>`),
	flip:       svgIcon(`<path d="M12 3.5v17"/><path d="M8.2 7.2 4.2 12l4 4.8V7.2z"/><path d="M15.8 7.2l4 4.8-4 4.8V7.2z"/>`),
	fill:       svgIcon(`<rect x="3.5" y="4.5" width="17" height="15" rx="2.6"/><rect x="12" y="11" width="6.4" height="6.2" rx="1.4" fill="currentColor" stroke="none"/>`),
	hud:        svgIcon(`<path d="M3.5 12h4l2.2-5.4 4.4 10.8 2.2-5.4h4.2"/>`),
	close:      svgIcon(`<path d="M6 6l12 12M18 6L6 18"/>`),
	check:      svgIcon(`<path d="M5 12.6l4.4 4.4L19 7.2"/>`),
	chevron:    svgIcon(`<path d="M6.5 9.2 12 14.8l5.5-5.6"/>`),
	alert:      svgIcon(`<path d="M12 4.2 21 20H3L12 4.2z"/><path d="M12 10.4v4.2"/><circle cx="12" cy="17.3" r="0.9" fill="currentColor" stroke="none"/>`),
	music:      svgIcon(`<path d="M9.2 17.4V6.2l10-1.7v11"/><circle cx="6.6" cy="17.8" r="2.7"/><circle cx="16.6" cy="15.9" r="2.7"/>`),
	external:   svgIcon(`<path d="M14 4.5h5.5V10"/><path d="M19.5 4.5 11 13"/><path d="M19.5 14.5v4A2.5 2.5 0 0 1 17 21H5.5A2.5 2.5 0 0 1 3 18.5V7A2.5 2.5 0 0 1 5.5 4.5h4"/>`),
	image:      svgIcon(`<rect x="3.5" y="4.5" width="17" height="15" rx="2.6"/><circle cx="9" cy="9.6" r="1.5"/><path d="M4.5 17l4.4-4.2 3.1 2.8 3.2-3.4 4.3 4.6"/>`),
	phone:      svgIcon(`<rect x="6.8" y="2.5" width="10.4" height="19" rx="2.6"/><path d="M10.6 5.2h2.8"/><circle cx="12" cy="18.2" r="0.9" fill="currentColor" stroke="none"/>`),
	backArrow:  svgIcon(`<path d="M15 4.5 7.5 12 15 19.5"/>`),
	homeIcon:   svgIcon(`<path d="M4 11.2 12 4l8 7.2"/><path d="M6.5 9.8v9.7h11V9.8"/>`),
	recents:    svgIcon(`<rect x="4.5" y="5" width="6" height="14" rx="1.6"/><rect x="13.5" y="5" width="6" height="14" rx="1.6"/>`),
	androidNav: svgIcon(`<rect x="3.5" y="6" width="17" height="13" rx="2.6"/><path d="M8 2.8 10 6M16 2.8 14 6"/><circle cx="9" cy="12" r="0.9" fill="currentColor" stroke="none"/><circle cx="15" cy="12" r="0.9" fill="currentColor" stroke="none"/>`),
}

// brandMark is the app's logo, drawn in its own colours: the amber monitor
// with the cream phone in front, as assets/logo.png has them.
var brandMark = ui.MustParseSVG([]byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 64 64">
<rect x="18" y="10" width="40" height="28" rx="7" fill="none" stroke="#E8A24A" stroke-width="4.5"/>
<rect x="6" y="23" width="18" height="31" rx="5.5" fill="#1B1815" stroke="#F4EFE7" stroke-width="4.5"/>
<path d="M24 38h27" fill="none" stroke="#E8A24A" stroke-width="4.5" stroke-linecap="round"/>
</svg>`))

// ---------- shared building blocks ----------

// heroRoot is the full-window ground of the idle screens: a warm glow from
// the top fading into the charcoal.
func heroRoot(c *ui.Context) ui.Element {
	return ui.Column(c).Fill().Background(colGround).LinearGradient(ui.LinearGradient{
		Stops: []ui.GradientStop{
			{Color: ui.Hex("#281E14"), At: 0},
			{Color: ui.Hex("#181411"), At: 0.42},
			{Color: colGround, At: 0.85},
		},
		Angle: 180,
		Oklab: true,
	})
}

// brandTile shows the logo in a floating tile, the anchor of every idle
// screen.
func brandTile(c *ui.Context, size float32) {
	ui.Box(c).Width(size).Height(size).Radius(size*0.26).Background(colSurface).
		Border(1, colBorder).InsetShadow(0, 1, 0, 0, colRim).
		Shadow(0, 16, 36, 0, ui.RGBA(0, 0, 0, 0.42)).Center().
		Children(func() {
			ui.Image(c, brandMark).Width(size * 0.58).Height(size * 0.58)
		})
}

// statusDot is the small coloured lamp that marks a screen's state.
func statusDot(c *ui.Context, col ui.Color, size float32) ui.Element {
	return ui.Box(c).Width(size).Height(size).Radius(size/2).Background(col).
		Shadow(0, 0, 9, 2, col.Alpha(0.5))
}

// pill is a rounded status chip.
func pill(c *ui.Context, text string, accent ui.Color) {
	ui.Text(c, text).FontSize(11.5).Bold().TextColor(accent).
		Padding(3.5, 10).Radius(999).Background(accent.Alpha(0.14)).
		Border(1, accent.Alpha(0.3)).SingleLine()
}

// statusPill is a pill with a lamp inside, for the hero screens.
func statusPill(c *ui.Context, text string, accent ui.Color) {
	ui.Row(c).Gap(7).AlignItems(ui.Center).Padding(5, 11).Radius(999).
		Background(accent.Alpha(0.1)).Border(1, accent.Alpha(0.26)).Children(func() {
		statusDot(c, accent, 7)
		ui.Text(c, text).FontSize(12).Bold().TextColor(accent)
	})
}

// softButton is a secondary action with a quiet face.
func softButton(c *ui.Context, key any, ic *ui.SVG, label string, danger bool, onClick func()) bool {
	b := ui.ButtonBase(c.Key(key)).Padding(7, 13).Radius(9).Gap(7)
	fg := colInk
	if danger {
		fg = colDanger
	}
	b.Background(colCardBg).Border(1, colBorder).TextColor(fg)
	if b.Hovered() {
		b.Background(ui.RGBA(255, 255, 255, 0.085))
	}
	if b.Pressed() {
		b.Background(ui.RGBA(255, 255, 255, 0.12))
	}
	b.Children(func() {
		if ic != nil {
			ui.Icon(c, ic).Width(15).Height(15)
		}
		ui.Text(c, label).FontSize(12.5)
	})
	clicked := b.Clicked()
	if clicked && onClick != nil {
		onClick()
	}
	return clicked
}

// primaryButton is the one accented action of a screen.
func primaryButton(c *ui.Context, key any, ic *ui.SVG, label string, onClick func()) bool {
	b := ui.PrimaryButton(c.Key(key), "").Padding(8, 15)
	b.Children(func() {
		ui.Row(c).Gap(7).AlignItems(ui.Center).Children(func() {
			if ic != nil {
				ui.Icon(c, ic).Width(15).Height(15)
			}
			ui.Text(c, label).FontSize(12.5).Bold()
		})
	})
	clicked := b.Clicked()
	if clicked && onClick != nil {
		onClick()
	}
	return clicked
}

// iconButton is a bare icon control for the live bar and window chrome. Its
// look is drawn here, because the theme's button face would cover the video
// with a slab on every frame.
func iconButton(c *ui.Context, key any, ic *ui.SVG, tip string, active, danger bool) ui.Element {
	b := ui.ButtonBase(c.Key(key)).Padding(6.5).Radius(8)
	switch {
	case active:
		b.Background(colAccent.Alpha(0.16)).TextColor(colAccent)
		if b.Hovered() {
			b.Background(colAccent.Alpha(0.24))
		}
	case danger:
		b.TextColor(colDanger)
		if b.Hovered() {
			b.Background(colDanger.Alpha(0.14))
		}
		if b.Pressed() {
			b.Background(colDanger.Alpha(0.2))
		}
	default:
		b.TextColor(colMuted)
		if b.Hovered() {
			b.Background(colHover)
			b.TextColor(colInk)
		}
		if b.Pressed() {
			b.Background(ui.RGBA(255, 255, 255, 0.12))
		}
	}
	if tip != "" {
		b.Tooltip(tip)
		b.Label(tip)
	}
	b.Children(func() { ui.Icon(c, ic).Width(17).Height(17) })
	return b
}

// idleChrome is the floating window-control cluster shared by all idle
// screens, so a hero screen has no heavy title bar.
func (a *mirrpaneApp) idleChrome(c *ui.Context) {
	ui.Row(c).Absolute().Top(14).Right(16).Gap(2).ZIndex(80).Children(func() {
		if iconButton(c, "chrome-pin", icons.pin, pinLabel(a.alwaysOnTop), a.alwaysOnTop, false).Clicked() {
			a.toggleAlwaysOnTop()
		}
		if iconButton(c, "chrome-fs", icons.fullscreen, fullscreenLabel(a.fullscreen), a.fullscreen, false).Clicked() {
			a.toggleFullScreen()
		}
		if iconButton(c, "chrome-logs", icons.logs, "日誌 (Ctrl+L)", false, false).Clicked() {
			a.openLogs()
		}
		if iconButton(c, "chrome-settings", icons.settings, "設定 (Ctrl+K)", false, false).Clicked() {
			a.openSettings()
		}
	})
}

// vrule separates groups of controls in the live bar.
func vrule(c *ui.Context) {
	ui.Box(c).Width(1).Height(18).Background(ui.RGBA(255, 244, 230, 0.16)).MarginX(3)
}

// menuPanel styles a popover menu's panel.
func menuPanel(panel ui.Element) {
	panel.Padding(6).Gap(2).Radius(12).Background(colMenuBg).
		Border(1, colBorder).InsetShadow(0, 1, 0, 0, colRim).
		Shadow(0, 18, 44, 0, ui.RGBA(0, 0, 0, 0.5))
}

// menuItem is one row of a popover menu.
func menuItem(c *ui.Context, key any, ic *ui.SVG, label, hint string, checked, danger bool, action func()) bool {
	row := ui.ButtonBase(c.Key(key)).FillWidth().Padding(7, 9).Radius(8).Gap(9)
	fg := colInk
	if danger {
		fg = colDanger
	}
	row.TextColor(fg)
	if row.Hovered() {
		row.Background(colSurfaceHi)
	}
	row.Children(func() {
		if ic != nil {
			ui.Icon(c, ic).Width(16).Height(16)
		}
		ui.Text(c, label).FontSize(13).Grow(1).SingleLine()
		if hint != "" {
			ui.Text(c, hint).FontSize(11).TextColor(colFaint)
		}
		if checked {
			ui.Icon(c, icons.check).Width(14).Height(14).TextColor(colAccent)
		}
	})
	clicked := row.Clicked()
	if clicked && action != nil {
		action()
	}
	return clicked
}

// heroHeader draws the shared identity of an idle screen: the mark, the
// name, and a one-line subtitle.
func heroHeader(c *ui.Context, title, subtitle string) {
	ui.Column(c).Gap(12).AlignItems(ui.Center).Children(func() {
		brandTile(c, 72)
		ui.Column(c).Gap(5).AlignItems(ui.Center).Children(func() {
			ui.Text(c, title).FontSize(36).Bold().TextColor(colInk).LetterSpacing(1.5)
			ui.Text(c, subtitle).FontSize(11.5).TextColor(colFaint).LetterSpacing(1.4)
		})
	})
}

// sectionLabel is the small muted heading of a settings section.
func sectionLabel(c *ui.Context, text string) {
	ui.Text(c, text).FontSize(11).Bold().TextColor(colFaint).LetterSpacing(1.4)
}

// card is the surface card used by the hero screens.
func card(c *ui.Context, width float32) ui.Element {
	return ui.Column(c).Width(width).Padding(22, 24).Gap(16).Radius(16).
		Background(colSurface).Border(1, colBorder).
		InsetShadow(0, 1, 0, 0, colRim).
		Shadow(0, 22, 56, 0, ui.RGBA(0, 0, 0, 0.48))
}

// well is a recessed container: the chassis colour inside a panel.
func well(c *ui.Context) ui.Element {
	return ui.Column(c).Background(colGround).Border(1, colHairline).Radius(10)
}

// colorIf picks one of two colours.
func colorIf(cond bool, yes, no ui.Color) ui.Color {
	if cond {
		return yes
	}
	return no
}
