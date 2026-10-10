package browser

import "time"

type closeWitness interface {
	stop(*Browser) error
	deadline() time.Time
}

var prepareBrowserClose = func(b *Browser, polite bool) closeWitness { return b.prepareClose(polite) }
