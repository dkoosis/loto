package main

import (
	"crypto/sha256"
	"encoding/binary"
	"strings"
)

// generateHandle maps a session UUID to a deterministic adjective-animal handle.
// The UUID bytes are hashed so any UUID format maps cleanly to the word space.
// Space: len(adjectives) × len(animals) = 10,000 combinations.
func generateHandle(uuid string) string {
	h := sha256.Sum256([]byte(uuid))
	adj := adjectives[binary.BigEndian.Uint32(h[0:4])%uint32(len(adjectives))] //nolint:gosec // G115: word lists are <1k entries
	animal := animals[binary.BigEndian.Uint32(h[4:8])%uint32(len(animals))]    //nolint:gosec // G115: word lists are <1k entries
	return toTitle(adj) + toTitle(animal)
}

func toTitle(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

var adjectives = []string{
	"able", "agile", "alert", "amber", "ample",
	"apt", "arch", "ardent", "arid", "avid",
	"azure", "bare", "bash", "bold", "brave",
	"brief", "bright", "brisk", "broad", "brown",
	"calm", "candid", "civic", "clean", "clear",
	"clever", "cold", "cool", "crisp", "cubic",
	"cyan", "dark", "deft", "dense", "deep",
	"dim", "direct", "dry", "dual", "dusk",
	"eager", "early", "east", "elder", "elite",
	"even", "exact", "fair", "fast", "feral",
	"firm", "fixed", "flat", "fleet", "focal",
	"fond", statusFree, "fresh", "frugal", "full",
	"gale", "gentle", "gold", "grand", "gray",
	"great", "green", "grim", "gross", "hardy",
	"harsh", "heady", "high", "hollow", "honest",
	"hyper", "idle", "inert", "inner", "ionic",
	"jade", "just", "keen", "kind", "large",
	"late", "lean", "light", "lithe", "lofty",
	"lone", "long", "loud", "lucid", "lunar",
	"meld", "mild", "mobile", "modest", "muted",
	"naval", "neat", "noble", "north", "null",
	"oaken", "odd", "olive", "open", "outer",
	"pale", "plain", "plaid", "polar", "prime",
	"proud", "pure", "quick", "quiet", "rapid",
	"raw", "ready", "real", "regal", "remote",
	"rigid", "rival", "rosy", "round", "royal",
	"rural", "safe", "sage", "sharp", "short",
	"silent", "silver", "sleek", "slim", "slow",
	"smart", "solar", "solid", "stable", "stark",
	"steel", "still", "stoic", "strict", "sturdy",
	"subtle", "sunny", "super", "sure", "swift",
	"tall", "tame", "taut", "terse", "tidal",
	"tight", "tired", "tonic", "tough", "true",
	"ultra", "urban", "valid", "vast", "vital",
	"vivid", "warm", "wild", "wise", "young",
}

var animals = []string{
	"adder", "albatross", "alpaca", "antelope", "ape",
	"armadillo", "aurochs", "aye-aye", "badger", "bat",
	"bear", "beaver", "bison", "boar", "bobcat",
	"bongo", "brant", "bream", "buck", "buffalo",
	"bullfrog", "bunting", "burro", "caiman", "caracal",
	"caribou", "cassowary", "cat", "cheetah", "chipmunk",
	"civet", "cobra", "condor", "cormorant", "cougar",
	"coyote", "crane", "crow", "dace", "dingo",
	"dove", "dragonfly", "drake", "dunlin", "eagle",
	"egret", "elk", "emu", "ermine", "falcon",
	"ferret", "finch", "fisher", "fox", "frog",
	"galah", "gecko", "genet", "gerbil", "gibbon",
	"gopher", "grebe", "grizzly", "grosbeak", "grouse",
	"gull", "hare", "harrier", "hawk", "heron",
	"horse", "hyena", "ibis", "iguana", "impala",
	"jaguar", "jay", "junco", "kestrel", "killdeer",
	"kite", "kiwi", "koala", "kookaburra", "lapwing",
	"lemur", "leopard", "limpet", "linnet", "lion",
	"lizard", "llama", "locust", "loon", "lynx",
	"magpie", "marmot", "marten", "martin", "meadowlark",
	"merlin", "mink", "mole", "mongoose", "moose",
	"moth", "mouse", "mule", "musk-ox", "narwhal",
	"newt", "nightjar", "nuthatch", "ocelot", "okapi",
	"opossum", "osprey", "otter", "owl", "panda",
	"panther", "parrot", "peregrine", "petrel", "pika",
	"pipit", "plover", "porcupine", "puffin", "quail",
	"rabbit", "raccoon", "raven", "redstart", "rhea",
	"robin", "rook", "sable", "salamander", "sandpiper",
	"seal", "shearwater", "shrike", "siskin", "skunk",
	"snipe", "sparrow", "squirrel", "starling", "stoat",
	"stork", "swallow", "swan", "swift", "tanager",
	"teal", "tern", "thrush", "toad", "toucan",
	"treecreeper", "trout", "turtle", "vole", "vulture",
	"warbler", "weasel", "wheatear", "widgeon", "wolf",
	"wolverine", "wombat", "woodcock", "woodpecker", "wren",
	"yak", "yellowthroat", "zebu", "zorilla", "zorro",
}
