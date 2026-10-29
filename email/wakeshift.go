package email

// LogoURL and DarkLogoURL are Wakeshift's logo for email, served by the
// site (site/static/email): an image on wakeshift.ai, never an attachment.
const (
	LogoURL     = "https://wakeshift.ai/static/email/wakeshift.png"
	DarkLogoURL = "https://wakeshift.ai/static/email/wakeshift-dark.png"
)

// Support is where every email sends questions.
const Support = "support@wakeshift.ai"

// Wakeshift is the core's brand: the site's Morning Paper palette.
var Wakeshift = Brand{
	Name:    "Wakeshift",
	HomeURL: "https://wakeshift.ai",
	Logo:    &Logo{URL: LogoURL, DarkURL: DarkLogoURL, Width: 162, Height: 32},
	Light: Colors{Page: "#F5F1E8", Card: "#FFFDF8", Border: "#DDD5C4", Heading: "#1D1B17", Text: "#4A463E", Muted: "#6B665B",
		Accent: "#C25418", Button: "#1D1B17", ButtonText: "#F5F1E8"},
	Dark: Colors{Page: "#161512", Card: "#1F1D19", Border: "#34312B", Heading: "#F5F1E8", Text: "#DDD5C4", Muted: "#A8A194",
		Accent: "#E89A6A", Button: "#F5F1E8", ButtonText: "#1D1B17"},
	HeadingFont:   `"Iowan Old Style", Charter, "Palatino Linotype", Palatino, Georgia, serif`,
	BodyFont:      `-apple-system, BlinkMacSystemFont, "Segoe UI", "Helvetica Neue", Arial, sans-serif`,
	KickerFont:    `ui-monospace, "SF Mono", Menlo, Consolas, monospace`,
	HeadingWeight: "400",
	KickerWeight:  "600",
	Sender:        "Sent by Wakeshift (Clawvisor Inc.)",
	Support:       Support,
}
