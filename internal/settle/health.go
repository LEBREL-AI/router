package settle

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Health is the backlog as an operator watches it. The age of the oldest operation owed is the
// number that matters: the web chat waits up to 10 s for a turn's settlement, and a customer's
// unsettled holds keep their concurrency slots, so a growing age is felt before anything fails.
// Operations only in memory (the journal could not take them) count as lagging too: a restart
// would lose them.
type Health struct {
	Status           string  `json:"status"` // "ok", or "lagging": the oldest owed operation is older than the alert age, or some are only in memory
	MemoryOnly       int     `json:"memory_only"`
	OldestAgeSeconds float64 `json:"oldest_age_seconds"`
	OverAlertAge     int     `json:"over_alert_age"`
	AlertAgeSeconds  float64 `json:"alert_age_seconds"`
	InFlight         int     `json:"in_flight"`
	Concurrency      int     `json:"concurrency"`
	Failing          int     `json:"failing"` // calls that failed in a row just now: the platform or the network, not capacity
	Probing          bool    `json:"probing"`
	BatchSize        int     `json:"batch_size"`
	Batching         bool    `json:"batching"`
	DeadOnDisk       int     `json:"dead_on_disk"`
	Stats
}

// Health measures the backlog now against the alert age.
func (q *Queue) Health(alertAge time.Duration) Health {
	now := q.options.Now()
	q.mu.Lock()
	defer q.mu.Unlock()
	var oldest time.Duration
	over := 0
	for _, op := range q.ops {
		age := now.Sub(op.CreatedAt)
		if age > oldest {
			oldest = age
		}
		if age > alertAge {
			over++
		}
	}
	h := Health{Status: "ok", MemoryOnly: len(q.memoryOnly), OldestAgeSeconds: oldest.Seconds(), OverAlertAge: over, AlertAgeSeconds: alertAge.Seconds(),
		InFlight: q.inFlight, Concurrency: q.options.Concurrency, Failing: q.failures, Probing: q.failures >= q.options.ProbeAfter, BatchSize: q.options.BatchSize,
		Batching: q.batcher != nil && !now.Before(q.batchOff), DeadOnDisk: q.deadOnDisk, Stats: q.stats}
	h.Stats.Pending = len(q.ops)
	if oldest > alertAge || h.MemoryOnly > 0 {
		h.Status = "lagging"
	}
	return h
}

func (q *Queue) drainDead() []DeadLetter {
	q.mu.Lock()
	defer q.mu.Unlock()
	dead := q.dead
	q.dead = nil
	return dead
}

func (q *Queue) drainNotices() []string {
	q.mu.Lock()
	defer q.mu.Unlock()
	notices := q.notices
	q.notices = nil
	return notices
}

// retryDisk writes again what only memory holds, when there is some.
func (q *Queue) retryDisk() {
	q.mu.Lock()
	waiting := len(q.memoryOnly) > 0 && q.journal != nil
	q.mu.Unlock()
	if waiting {
		q.catchUp()
	}
}

const (
	reminderEvery = 10 * time.Minute
	// the calls in flight, averaged over the last minute, against the concurrency bound: past
	// busyAbove the queue is close to its ceiling, under busyBelow it has room again
	busyAbove = 0.75
	busyBelow = 0.5
	// A delay that is not capacity (calls failing, or slots to spare) is the platform or the network:
	// every operation is on disk and retried on its own, so it is told only once it has lasted this
	// long. On 26 Oct 2026 a moment of packet loss hung one instance's settlement connection for 50 s,
	// with 1 of 8 calls in use, and the alert asked for an instance nobody needed.
	stalledAfter = 2 * time.Minute
)

// Watch checks the backlog every “every“ and tells notify once per change. Busy comes first: the
// calls in flight have averaged three quarters of the bound over the last minute while they answer,
// so the ceiling is near while nothing is late yet; that is when to add an instance. Lagging: the
// oldest owed operation is older than alertAge at two checks in a row, when that is capacity (every
// call in use and answering) or a risk (operations only in memory); a delay with calls failing or
// slots to spare is the platform or the network and is told once the oldest has waited stalledAfter.
// Recovered when it is back under at two checks in a row, a reminder every 10 minutes while it lags. And every refusal the control plane gave since
// the last check. It alone writes again what only memory holds (catchUp). When ctx ends it makes
// one last pass (the disk, the disk alerts, the refusals) before it returns, so a stopping router
// still tells what happened in its last seconds. The texts are for the founder, in Spanish.
func (q *Queue) Watch(ctx context.Context, every, alertAge time.Duration, notify func(string)) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	over, under := 0, 0
	lagging, busy := false, false
	var reminded time.Time
	window := min(120, max(2, int(time.Minute/every)))
	var inFlight []int
	for {
		select {
		case <-ctx.Done():
			q.retryDisk()
			for _, notice := range q.drainNotices() {
				notify(notice)
			}
			if dead := q.drainDead(); len(dead) > 0 {
				notify(deadText(dead, q.Health(alertAge).DeadOnDisk))
			}
			return
		case <-ticker.C:
		}
		q.retryDisk()
		health := q.Health(alertAge)
		for _, notice := range q.drainNotices() {
			notify(notice)
		}
		if dead := q.drainDead(); len(dead) > 0 {
			notify(deadText(dead, health.DeadOnDisk))
		}
		if inFlight = append(inFlight, health.InFlight); len(inFlight) > window {
			inFlight = inFlight[1:]
		}
		sum := 0
		for _, value := range inFlight {
			sum += value
		}
		use := float64(sum) / float64(len(inFlight)) / float64(health.Concurrency)
		switch {
		case !busy && len(inFlight) == window && use >= busyAbove && health.Failing == 0:
			busy = true
			state := "todavía sin retraso. Toca añadir una instancia antes de que se acumule"
			if health.Status == "lagging" {
				state = "y ya con retraso. Toca añadir una instancia"
			}
			notify(fmt.Sprintf("el router va al %.0f %% de su capacidad de liquidación (media del último minuto: %.0f de %d llamadas en curso), %s (docs/scaling.md).",
				use*100, use*float64(health.Concurrency), health.Concurrency, state))
		case busy && use < busyBelow:
			busy = false
			notify(fmt.Sprintf("la capacidad de liquidación vuelve a tener margen: %.0f %% de uso en el último minuto.", use*100))
		}
		// once told, a delay counts until it is gone; before, only what someone should act on counts
		capacity := health.InFlight >= health.Concurrency && health.Failing == 0
		worth := lagging || health.MemoryOnly > 0 || capacity || health.OldestAgeSeconds >= max(stalledAfter, alertAge).Seconds()
		if health.Status == "lagging" && worth {
			over, under = over+1, 0
		} else if health.Status != "lagging" {
			over, under = 0, under+1
		} else {
			over, under = 0, 0
		}
		now := q.options.Now()
		switch {
		case !lagging && over >= 2:
			lagging, reminded = true, now
			notify(laggingText(health))
		case lagging && under >= 2:
			lagging = false
			notify(fmt.Sprintf("liquidaciones al día otra vez: %d pendientes, la más antigua %s.", health.Pending, seconds(health.OldestAgeSeconds)))
		case lagging && now.Sub(reminded) >= reminderEvery:
			reminded = now
			notify(laggingText(health))
		}
	}
}

func laggingText(h Health) string {
	text := fmt.Sprintf("las liquidaciones van con retraso: %d pendientes, la más antigua lleva %s (umbral %s), %d de %d llamadas en curso",
		h.Pending, seconds(h.OldestAgeSeconds), seconds(h.AlertAgeSeconds), h.InFlight, h.Concurrency)
	switch {
	case h.MemoryOnly > 0:
		text += fmt.Sprintf("; %d solo en memoria porque el diario no se escribe: hay que revisar el disco del servidor, un reinicio ahora las perdería", h.MemoryOnly)
	case h.Probing:
		text += "; la plataforma no responde: el router le manda una sola llamada de prueba cada vez hasta que conteste. Los cobros están en disco y se entregan solos cuando vuelva"
	case h.Failing > 0:
		text += fmt.Sprintf("; %d llamadas seguidas sin respuesta: lebrel.ai no contesta a esta instancia o falla la red del servidor, no es capacidad. Los cobros están en disco y se reintentan solos", h.Failing)
	case h.InFlight >= h.Concurrency:
		text += ". Todas las llamadas están ocupadas y contestando: toca añadir una instancia del router (docs/scaling.md)"
	default:
		text += "; quedan llamadas libres, así que no es capacidad: lebrel.ai tarda en contestar. Los cobros están en disco y se reintentan solos"
	}
	return text + "."
}

func deadText(dead []DeadLetter, onDisk int) string {
	parts := make([]string, 0, 5)
	for i, letter := range dead {
		if i == 5 {
			parts = append(parts, fmt.Sprintf("y %d más", len(dead)-5))
			break
		}
		var why string
		switch letter.Code {
		case "unreadable_record":
			why = "registro ilegible en disco"
		case "invalid_operation":
			why = "no se puede enviar tal como está"
		case "":
			why = "rechazada por la plataforma, sin código"
		default:
			why = "rechazada por la plataforma: " + letter.Code
		}
		parts = append(parts, fmt.Sprintf("%s %s (%s)", letter.RequestID, letter.Kind, why))
	}
	head := fmt.Sprintf("%d operaciones de cobro no se pueden entregar", len(dead))
	if len(dead) == 1 {
		head = "1 operación de cobro no se puede entregar"
	}
	unsaved := 0
	for _, letter := range dead {
		if !letter.Saved {
			unsaved++
		}
	}
	where := fmt.Sprintf("guardadas en dead/ para revisarlas a mano (%d en total)", onDisk)
	if unsaved > 0 {
		where += fmt.Sprintf("; %d no caben en dead/ porque el disco falla: siguen pendientes y vuelven a salir en el próximo arranque", unsaved)
	}
	return fmt.Sprintf("%s; %s: %s.", head, where, strings.Join(parts, "; "))
}

func seconds(value float64) string {
	if value < 10 {
		return strings.Replace(fmt.Sprintf("%.1f s", value), ".", ",", 1)
	}
	return fmt.Sprintf("%.0f s", value)
}
