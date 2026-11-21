package workspace

// ActivePreparations reports how many preparations hold a slot.
func ActivePreparations(p *Prepare) int { return len(p.slots) }
