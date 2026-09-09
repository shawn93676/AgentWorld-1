package village

// seed.go —— 世界种子数据：地点图 + 10 个村民人设（对应 G0 §3/§4）。
// 初始关系与债务是刻意埋的"故事引子"（Bob 欠钱、John 的店铺梦）。

// initPlaces 初始化 8 个地点（G0 §3 地图）。X/Y 为前端 SVG 布局坐标。
func (w *World) initPlaces() {
	ps := []*Place{
		{ID: "castle", Name: "Castle", Emoji: "🏰", Desc: "The lord's keep. Marco stands guard here.",
			X: 180, Y: 40, Links: []string{"town_square"}},
		{ID: "town_square", Name: "Town Square", Emoji: "⛲", Desc: "The heart of the village. News and rumors gather here.",
			X: 180, Y: 125, Links: []string{"castle", "tavern", "market", "church", "residential"}},
		{ID: "tavern", Name: "Tavern", Emoji: "🍺", Desc: "Mary's warm hearth. Ale, stew and gossip.",
			X: 72, Y: 215, Links: []string{"town_square", "residential"}},
		{ID: "market", Name: "Market", Emoji: "🛒", Desc: "Stalls of goods and haggling voices.",
			X: 180, Y: 215, Links: []string{"town_square", "residential"}},
		{ID: "church", Name: "Church", Emoji: "⛪", Desc: "Quiet candles and Grace's gentle voice.",
			X: 288, Y: 215, Links: []string{"town_square", "residential"}},
		{ID: "residential", Name: "Residential", Emoji: "🏠", Desc: "Cottages where villagers live and rest.",
			X: 180, Y: 300, Links: []string{"town_square", "tavern", "market", "church", "mine", "farm"}},
		{ID: "mine", Name: "Mine", Emoji: "⛏️", Desc: "Dark tunnels rich with iron — and hope.",
			X: 100, Y: 395, Links: []string{"residential", "farm"}},
		{ID: "farm", Name: "Farm", Emoji: "🌾", Desc: "Tom's wheat fields and a stubborn old cow.",
			X: 260, Y: 395, Links: []string{"residential", "mine"}},
	}
	for _, p := range ps {
		w.places[p.ID] = p
		w.placeOrd = append(w.placeOrd, p.ID)
	}
}

// Places 按固定顺序返回地点（含相邻 id，供前端画边）。
func (w *World) Places() []*Place {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]*Place, 0, len(w.placeOrd))
	for _, id := range w.placeOrd {
		out = append(out, w.places[id])
	}
	return out
}

// Profile 村民人设（种子数据）。
type Profile struct {
	Name        string
	Occupation  string
	Emoji       string
	Personality []string
	Money       int64
	Skills      map[string]int
	Goal        string
	GoalTarget  int64 // 攒钱类目标的金额（0 = 非攒钱目标）
	Workplace   string
	BedHour     int
	WakeHour    int
	Social      float64
	Grit        float64
	Lines       []string // 闲聊模板句
	SeedRel     map[string]struct {
		Like  int
		Trust int
	}
	Owes    map[string]int64 // 初始债务（欠谁多少）
	SeedMem []string
}

// Profiles 10 个村民（V0.1 固定规模，G0 §2）。
var Profiles = []Profile{
	{
		Name: "John", Occupation: "Blacksmith", Emoji: "⚒️",
		Personality: []string{"Hardworking", "Proud", "Honest", "Short-tempered"},
		Money:       127,
		Skills:      map[string]int{"Blacksmithing": 72, "Trading": 31},
		Goal:        "Open my own shop", GoalTarget: 500,
		Workplace: "mine", BedHour: 22, WakeHour: 6, Social: 0.5, Grit: 0.6,
		Lines: []string{
			"A sword doesn't forgive a sloppy strike. Neither do I.",
			"These hands built every hinge in this village.",
			"Iron is honest. It bends exactly as much as it must.",
		},
		SeedRel: map[string]struct {
			Like  int
			Trust int
		}{
			"Mary": {Like: 72, Trust: 81}, "Bob": {Like: 18, Trust: 15}, "Tom": {Like: 12, Trust: 30},
			"William": {Like: 55, Trust: 50},
		},
		Owes: map[string]int64{},
		SeedMem: []string{
			"Bob borrowed 50 gold from me and still hasn't repaid it.",
			"Mary helped me when I was sick last winter.",
			"I will open my own shop, no matter how long it takes.",
		},
	},
	{
		Name: "Mary", Occupation: "Innkeeper", Emoji: "🍺",
		Personality: []string{"Warm", "Chatty", "Shrewd"},
		Money:       215,
		Skills:      map[string]int{"Cooking": 68, "Trading": 45},
		Goal:        "Expand the Tavern", GoalTarget: 400,
		Workplace: "tavern", BedHour: 23, WakeHour: 5, Social: 0.9, Grit: 0.3,
		Lines: []string{
			"Sit down, Traveler — the stew is fresh and the news is fresher.",
			"Everyone talks in my tavern. You just have to listen.",
			"A full room and a warm oven: that's a good day.",
		},
		SeedRel: map[string]struct {
			Like  int
			Trust int
		}{
			"John": {Like: 81, Trust: 76}, "Alice": {Like: 63, Trust: 70},
			"Bob": {Like: 50, Trust: 45}, "Emma": {Like: 58, Trust: 55},
		},
		Owes: map[string]int64{},
		SeedMem: []string{
			"John helped repair my roof after the storm. I owe him one.",
			"Bob drinks more than he pays. Keep an eye on his tab.",
		},
	},
	{
		Name: "Bob", Occupation: "Merchant", Emoji: "🛒",
		Personality: []string{"Ambitious", "Risk-taking", "Anxious"},
		Money:       45,
		Skills:      map[string]int{"Trading": 58, "Haggling": 62},
		Goal:        "Repay my debts and restock", GoalTarget: 0,
		Workplace: "market", BedHour: 23, WakeHour: 7, Social: 0.8, Grit: 0.4,
		Lines: []string{
			"Buy low, sell high — the rest is just patience.",
			"The caravan was late again. My margins are bleeding.",
			"Every coin I owe is a coin that owns me.",
		},
		SeedRel: map[string]struct {
			Like  int
			Trust int
		}{
			"John": {Like: 35, Trust: 30}, "Mary": {Like: 55, Trust: 40},
			"Tom": {Like: 45, Trust: 50}, "Emma": {Like: 40, Trust: 35},
		},
		Owes: map[string]int64{"John": 50, "Tom": 50},
		SeedMem: []string{
			"I owe John 50 gold. He won't forget it. Neither will I.",
			"I borrowed 50 gold from Tom after the fire. I must repay it.",
		},
	},
	{
		Name: "Tom", Occupation: "Farmer", Emoji: "🌾",
		Personality: []string{"Calm", "Generous", "Slow"},
		Money:       80,
		Skills:      map[string]int{"Farming": 65, "Animal": 40},
		Goal:        "Buy a milk cow", GoalTarget: 200,
		Workplace: "farm", BedHour: 21, WakeHour: 6, Social: 0.4, Grit: 0.3,
		Lines: []string{
			"The wheat doesn't rush, and neither do I.",
			"Rain tomorrow, maybe. My knee says yes.",
			"Bob's a fool with coins, but he's a good lad at heart.",
		},
		SeedRel: map[string]struct {
			Like  int
			Trust int
		}{
			"Bob": {Like: 45, Trust: 50}, "Mary": {Like: 55, Trust: 60},
			"Grace": {Like: 60, Trust: 65},
		},
		Owes: map[string]int64{},
		SeedMem: []string{
			"I lent Bob 50 gold. He promised to repay after the harvest.",
			"Mary gives me leftover bread for the hens.",
		},
	},
	{
		Name: "Alice", Occupation: "Healer", Emoji: "🌿",
		Personality: []string{"Gentle", "Curious", "Devoted"},
		Money:       160,
		Skills:      map[string]int{"Herbalism": 70, "Medicine": 55},
		Goal:        "Build a small apothecary", GoalTarget: 250,
		Workplace: "church", BedHour: 22, WakeHour: 6, Social: 0.6, Grit: 0.4,
		Lines: []string{
			"Moon-herb only blooms at dusk. Patience is the first medicine.",
			"John coughs like a man swallowing iron dust. I should bring him tea.",
			"Every wound teaches the hands that close it.",
		},
		SeedRel: map[string]struct {
			Like  int
			Trust int
		}{
			"Mary": {Like: 63, Trust: 70}, "Grace": {Like: 70, Trust: 75},
			"John": {Like: 40, Trust: 45},
		},
		Owes: map[string]int64{},
		SeedMem: []string{
			"Mary taught me her grandmother's cough remedy.",
			"I need moon-herb and spring water for the apothecary tinctures.",
		},
	},
	{
		Name: "Emma", Occupation: "Baker", Emoji: "🥖",
		Personality: []string{"Cheerful", "Early-riser", "Talkative"},
		Money:       140,
		Skills:      map[string]int{"Baking": 66, "Trading": 38},
		Goal:        "Open a real bakery storefront", GoalTarget: 350,
		Workplace: "market", BedHour: 21, WakeHour: 4, Social: 0.85, Grit: 0.35,
		Lines: []string{
			"The dough rose perfectly today. A sign, surely!",
		"Honey bread out of the oven in ten minutes — you'll smell it before I say it.",
			"Everyone smiles at a baker. It's the best part of the job.",
		},
		SeedRel: map[string]struct {
			Like  int
			Trust int
		}{
			"Mary": {Like: 58, Trust: 55}, "Tom": {Like: 50, Trust: 55},
			"Bob": {Like: 40, Trust: 35},
		},
		Owes: map[string]int64{},
		SeedMem: []string{
			"Tom sells me the best flour in the village.",
			"A bakery storefront needs 350 gold. I'm getting there, crumb by crumb.",
		},
	},
	{
		Name: "William", Occupation: "Miner", Emoji: "⛏️",
		Personality: []string{"Tough", "Quiet", "Stubborn"},
		Money:       95,
		Skills:      map[string]int{"Mining": 75, "Blacksmithing": 30},
		Goal:        "Find a gold vein", GoalTarget: 0,
		Workplace: "mine", BedHour: 22, WakeHour: 5, Social: 0.25, Grit: 0.8,
		Lines: []string{
			"The deep tunnel talks. You just have to hit it right.",
			"Rock doesn't lie. People do.",
			"One more vein. I can feel it in the pickaxe.",
		},
		SeedRel: map[string]struct {
			Like  int
			Trust int
		}{
			"John": {Like: 55, Trust: 50}, "Marco": {Like: 35, Trust: 40},
		},
		Owes: map[string]int64{},
		SeedMem: []string{
			"John buys every good ore I pull out. Fair prices, no haggling.",
			"The east shaft groans at night. Someday it'll give up its gold.",
		},
	},
	{
		Name: "Grace", Occupation: "Priest", Emoji: "🔔",
		Personality: []string{"Kind", "Wise", "Forgiving"},
		Money:       120,
		Skills:      map[string]int{"Faith": 80, "Healing": 40},
		Goal:        "Restore the cracked church bell", GoalTarget: 300,
		Workplace: "church", BedHour: 22, WakeHour: 5, Social: 0.7, Grit: 0.5,
		Lines: []string{
			"The bell cracked, but the faith didn't.",
			"Even Bob is worth three prayers a week.",
			"Come in, Traveler. The candles are lit for everyone.",
		},
		SeedRel: map[string]struct {
			Like  int
			Trust int
		}{
			"Alice": {Like: 70, Trust: 75}, "Tom": {Like: 60, Trust: 65},
			"Mary": {Like: 60, Trust: 60},
		},
		Owes: map[string]int64{},
		SeedMem: []string{
			"The bell needs 300 gold to recast. The village deserves its voice back.",
			"Alice's tinctures have saved three parishioners this spring.",
		},
	},
	{
		Name: "Marco", Occupation: "Guard", Emoji: "🛡️",
		Personality: []string{"Dutiful", "Stern", "Loyal"},
		Money:       110,
		Skills:      map[string]int{"Combat": 68, "Observation": 55},
		Goal:        "Become Captain of the Guard", GoalTarget: 0,
		Workplace: "castle", BedHour: 23, WakeHour: 6, Social: 0.45, Grit: 0.7,
		Lines: []string{
			"No trouble on my watch. Not once, not ever.",
			"The lord's boots are polished. Mine had better be too.",
			"I count every cart that leaves the gate. Habit.",
		},
		SeedRel: map[string]struct {
			Like  int
			Trust int
		}{
			"John": {Like: 45, Trust: 50}, "William": {Like: 35, Trust: 40},
			"Grace": {Like: 50, Trust: 55},
		},
		Owes: map[string]int64{},
		SeedMem: []string{
			"The Captain's post retires next spring. I must be ready.",
			"John's gate hinges never squeak. Good work deserves good pay.",
		},
	},
	{
		Name: "Nina", Occupation: "Tailor", Emoji: "🧵",
		Personality: []string{"Creative", "Perfectionist", "Shy"},
		Money:       88,
		Skills:      map[string]int{"Sewing": 64, "Design": 52},
		Goal:        "Sew a gown for the Castle ball", GoalTarget: 150,
		Workplace: "residential", BedHour: 22, WakeHour: 7, Social: 0.35, Grit: 0.5,
		Lines: []string{
			"Silk from the caravan would make the perfect gown... if only.",
			"A crooked stitch keeps me up at night.",
			"The Castle ball is in two months. My dress will be remembered.",
		},
		SeedRel: map[string]struct {
			Like  int
			Trust int
		}{
			"Mary": {Like: 52, Trust: 50}, "Emma": {Like: 48, Trust: 45},
			"Alice": {Like: 45, Trust: 45},
		},
		Owes: map[string]int64{},
		SeedMem: []string{
			"I need 150 gold for imported silk before the Castle ball.",
			"Emma traded me warm bread for a mended apron. Fair trade.",
		},
	},
}
