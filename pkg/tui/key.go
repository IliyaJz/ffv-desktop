package tui

import (
	"context"
	"io"
	"time"
)

type KeyType int

const (
	KeyNone KeyType = iota
	KeyRune
	KeyUp
	KeyDown
	KeyLeft
	KeyRight
	KeyPageUp
	KeyPageDown
	KeyHome
	KeyEnd
	KeyEnter
	KeySpace
	KeyTab
	KeyBackTab
	KeyBackspace
	KeyDelete
	KeyEsc
	KeyCtrlC
	KeyCtrlD
	KeyCtrlQ
)

type KeyEvent struct {
	Type KeyType
	Rune rune
}

func readKeyEvents(ctx context.Context, r io.Reader, events chan<- KeyEvent) {
	buf := make([]byte, 64)
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		n, err := r.Read(buf)
		if err != nil {
			return
		}
		if n == 0 {
			continue
		}

		i := 0
		for i < n {
			b := buf[i]

			switch {
			case b == 0x03: // Ctrl+C
				events <- KeyEvent{Type: KeyCtrlC}
				i++
			case b == 0x04: // Ctrl+D
				events <- KeyEvent{Type: KeyCtrlD}
				i++
			case b == 0x11: // Ctrl+Q
				events <- KeyEvent{Type: KeyCtrlQ}
				i++
			case b == 0x09: // Tab
				events <- KeyEvent{Type: KeyTab}
				i++
			case b == 0x0A || b == 0x0D: // Enter (LF or CR)
				events <- KeyEvent{Type: KeyEnter}
				i++
			case b == 0x20: // Space
				events <- KeyEvent{Type: KeySpace, Rune: ' '}
				i++
			case b == 0x7F || b == 0x08: // Backspace
				events <- KeyEvent{Type: KeyBackspace}
				i++
			case b == 0x1B: // Escape sequence
				if i+1 >= n {
					// Standalone ESC key
					events <- KeyEvent{Type: KeyEsc}
					i++
					continue
				}

				if buf[i+1] == '[' {
					if i+2 >= n {
						events <- KeyEvent{Type: KeyEsc}
						i += 2
						continue
					}

					switch buf[i+2] {
					case 'A': // Up
						events <- KeyEvent{Type: KeyUp}
						i += 3
					case 'B': // Down
						events <- KeyEvent{Type: KeyDown}
						i += 3
					case 'C': // Right
						events <- KeyEvent{Type: KeyRight}
						i += 3
					case 'D': // Left
						events <- KeyEvent{Type: KeyLeft}
						i += 3
					case 'H': // Home
						events <- KeyEvent{Type: KeyHome}
						i += 3
					case 'F': // End
						events <- KeyEvent{Type: KeyEnd}
						i += 3
					case 'Z': // Shift-Tab
						events <- KeyEvent{Type: KeyBackTab}
						i += 3
					case '1', '2', '3', '4', '5', '6', '7', '8':
						if i+3 < n && buf[i+3] == '~' {
							switch buf[i+2] {
							case '1', '7':
								events <- KeyEvent{Type: KeyHome}
							case '3':
								events <- KeyEvent{Type: KeyDelete}
							case '4', '8':
								events <- KeyEvent{Type: KeyEnd}
							case '5':
								events <- KeyEvent{Type: KeyPageUp}
							case '6':
								events <- KeyEvent{Type: KeyPageDown}
							}
							i += 4
						} else {
							events <- KeyEvent{Type: KeyEsc}
							i += 3
						}
					default:
						events <- KeyEvent{Type: KeyEsc}
						i += 3
					}
				} else {
					events <- KeyEvent{Type: KeyEsc}
					i++
				}
			case b >= 32 && b < 127: // Printable ASCII
				events <- KeyEvent{Type: KeyRune, Rune: rune(b)}
				i++
			default:
				// UTF-8 or ignored control char
				events <- KeyEvent{Type: KeyRune, Rune: rune(b)}
				i++
			}
		}
	}
}

func PollKeys(ctx context.Context, r io.Reader) <-chan KeyEvent {
	ch := make(chan KeyEvent, 32)
	go readKeyEvents(ctx, r, ch)
	return ch
}

func SleepWithContext(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}
