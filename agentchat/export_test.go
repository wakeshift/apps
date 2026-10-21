package agentchat

// SetRoomCode replaces shared channels' random code, returning a func that
// restores it.
func SetRoomCode(f func() string) (restore func()) {
	old := roomCode
	roomCode = f
	return func() { roomCode = old }
}
