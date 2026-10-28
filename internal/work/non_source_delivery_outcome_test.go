package work

import (
	"encoding/json"
	"testing"
	"time"

	"compound/internal/envelope"
)

func TestPrivateRuntimeDeliveryRejectsBrokenConsumerLinksWithFreshDigests(t *testing.T) {
	for _, kind := range []string{"action", "receipt after", "report after", "body after", "summary", "transition evidence", "receipt evidence", "second order"} {
		t.Run(kind, func(t *testing.T) {
			f := newPrivateDeliveryFixture(t)
			var transition envelope.Transition
			var receipt envelope.Receipt
			if err := f.s.readEventJSON(f.evidence.Consumer.TransitionID, &transition); err != nil {
				t.Fatal(err)
			}
			if err := f.s.readEventJSON(f.evidence.Consumer.ReceiptID, &receipt); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "action":
				transition.ActionRefs = []string{"act_unrelated"}
			case "receipt after":
				transition.AfterRefs = []string{f.evidence.Consumer.ReportRef, f.evidence.Files[0].AfterArtifact}
			case "report after":
				transition.AfterRefs = []string{receipt.ID, f.evidence.Files[0].AfterArtifact}
			case "body after":
				transition.AfterRefs = []string{receipt.ID, f.evidence.Consumer.ReportRef}
			case "summary":
				transition.StateDelta = "different observation"
			case "transition evidence":
				transition.EvidenceRefs = nil
			case "receipt evidence":
				receipt.EvidenceRefs = nil
			case "second order":
				transition.SecondOrder.FutureBehaviorChanged = true
			}
			// Corruption cannot be produced by RecordReceiptedAction. Bypass that
			// owner deliberately, then recapture hashes: custody alone is not a chain.
			for id, value := range map[string]any{transition.ID: transition, receipt.ID: receipt} {
				if err := f.s.replaceEvent(id, value); err != nil {
					t.Fatal(err)
				}
			}
			f.evidence.Consumer.TransitionSHA256 = deliveryTestDigest(t, transition)
			f.evidence.Consumer.ReceiptSHA256 = deliveryTestDigest(t, receipt)
			body, err := json.Marshal(f.evidence)
			if err != nil {
				t.Fatal(err)
			}
			ref, err := f.artifacts.PutPrivate(body)
			if err != nil {
				t.Fatal(err)
			}
			in := f.bindingInput()
			in.NonSourceDelivery.EvidenceRef = ref
			if _, err := f.s.Advance(f.w.ID, in, fixedNow.Add(5*time.Minute)); err == nil {
				t.Fatal("accepted broken consumer chain with recaptured hashes")
			}
		})
	}
}
