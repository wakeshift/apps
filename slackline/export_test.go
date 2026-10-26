package slackline

// CachedPeople is how many Slack profiles the app holds in memory (tests).
func (a *App) CachedPeople() int {
	n := 0
	a.people.Range(func(any, any) bool { n++; return true })
	return n
}
