package contract

import (
	"compound/internal/envelope"
	"compound/internal/mem"
	"compound/internal/product"
)

const productEssenceRefPrefix = "product-essence://"

// productEssenceBlock is the essence of the product a product checkout belongs
// to, folded from the owned ledger. It is served only when the product recall
// fence resolved the checkout to exactly one product, so a sibling product's
// essence can never reach the pack.
type productEssenceBlock struct {
	Entity  string
	Summary string
}

func productEssenceForFence(view *mem.CurrentView, fence productRecallFence) (productEssenceBlock, bool) {
	if view == nil || !fence.Required || fence.Status != "resolved" || fence.Entity == "" {
		return productEssenceBlock{}, false
	}
	record, ok := product.EssenceFromView(view, fence.Entity)
	if !ok {
		return productEssenceBlock{}, false
	}
	return productEssenceBlock{Entity: fence.Entity, Summary: product.EssenceSummary(record.Essence)}, true
}

// appendProductEssence adds the short essence block — audience, problem, goal,
// stage, constraints — to a product-repo ContextPack. Display context only:
// it never changes hooks, decisions, required checks, or routing.
func appendProductEssence(pack envelope.ContextPack, block productEssenceBlock, ok bool) envelope.ContextPack {
	if !ok {
		return pack
	}
	pack.Included = append(pack.Included, envelope.IncludedRef{
		Ref:         productEssenceRefPrefix + block.Entity,
		SourceClass: "runtime",
		Reason:      "product essence folded from Compound product facts (essence.*); the invariant a change in this checkout must not break",
		Confidence:  1,
		Status:      "current",
	})
	pack.SummaryMD += " Product essence (" + block.Entity + "): " + block.Summary + "."
	return pack
}
