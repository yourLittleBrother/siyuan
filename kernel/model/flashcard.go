// SiYuan - Build Your Eternal Digital Garden
// Copyright (c) 2020-present, b3log.org
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <https://www.gnu.org/licenses/>.

package model

import (
	"errors"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/88250/gulu"
	"github.com/88250/lute/ast"
	"github.com/88250/lute/parse"
	"github.com/siyuan-note/logging"
	"github.com/siyuan-note/riff"
	"github.com/siyuan-note/siyuan/kernel/cache"
	"github.com/siyuan-note/siyuan/kernel/filesys"
	"github.com/siyuan-note/siyuan/kernel/sql"
	"github.com/siyuan-note/siyuan/kernel/treenode"
	"github.com/siyuan-note/siyuan/kernel/util"
)

var Decks = map[string]*riff.Deck{}
var deckLock = sync.Mutex{}

func GetNotebookFlashcards(boxID string, page int) (blocks []*Block, total, pageCount int) {
	blocks = []*Block{}

	entries, err := os.ReadDir(filepath.Join(util.DataDir, boxID))
	if nil != err {
		logging.LogErrorf("read dir failed: %s", err)
		return
	}

	var rootIDs []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		if !strings.HasSuffix(entry.Name(), ".sy") {
			continue
		}

		rootIDs = append(rootIDs, strings.TrimSuffix(entry.Name(), ".sy"))
	}

	treeBlockIDs := map[string]bool{}
	for _, rootID := range rootIDs {
		blockIDs := getTreeSubTreeChildBlocks(rootID)
		for blockID, _ := range blockIDs {
			treeBlockIDs[blockID] = true
		}
	}

	deck := Decks[builtinDeckID]
	if nil == deck {
		return
	}

	var allBlockIDs []string
	deckBlockIDs := deck.GetBlockIDs()
	for _, blockID := range deckBlockIDs {
		if treeBlockIDs[blockID] {
			allBlockIDs = append(allBlockIDs, blockID)
		}
	}
	allBlockIDs = gulu.Str.RemoveDuplicatedElem(allBlockIDs)
	cards := deck.GetCardsByBlockIDs(allBlockIDs)

	blocks, total, pageCount = getCardsBlocks(cards, page)
	return
}

func GetTreeFlashcards(rootID string, page int) (blocks []*Block, total, pageCount int) {
	blocks = []*Block{}
	deck := Decks[builtinDeckID]
	if nil == deck {
		return
	}

	var allBlockIDs []string
	deckBlockIDs := deck.GetBlockIDs()
	treeBlockIDs := getTreeSubTreeChildBlocks(rootID)
	for _, blockID := range deckBlockIDs {
		if treeBlockIDs[blockID] {
			allBlockIDs = append(allBlockIDs, blockID)
		}
	}
	allBlockIDs = gulu.Str.RemoveDuplicatedElem(allBlockIDs)
	cards := deck.GetCardsByBlockIDs(allBlockIDs)

	blocks, total, pageCount = getCardsBlocks(cards, page)
	return
}

func GetFlashcards(deckID string, page int) (blocks []*Block, total, pageCount int) {
	blocks = []*Block{}
	var cards []riff.Card
	if "" == deckID {
		for _, deck := range Decks {
			blockIDs := deck.GetBlockIDs()
			cards = append(cards, deck.GetCardsByBlockIDs(blockIDs)...)
		}
	} else {
		deck := Decks[deckID]
		if nil == deck {
			return
		}

		blockIDs := deck.GetBlockIDs()
		cards = append(cards, deck.GetCardsByBlockIDs(blockIDs)...)
	}

	blocks, total, pageCount = getCardsBlocks(cards, page)
	return
}

func getCardsBlocks(cards []riff.Card, page int) (blocks []*Block, total, pageCount int) {
	const pageSize = 20
	total = len(cards)
	pageCount = int(math.Ceil(float64(total) / float64(pageSize)))
	start := (page - 1) * pageSize
	end := page * pageSize
	if start > len(cards) {
		start = len(cards)
	}
	if end > len(cards) {
		end = len(cards)
	}

	cards = cards[start:end]
	if 1 > len(cards) {
		blocks = []*Block{}
		return
	}

	var blockIDs []string
	for _, card := range cards {
		blockIDs = append(blockIDs, card.BlockID())
	}
	sort.Strings(blockIDs)

	sqlBlocks := sql.GetBlocks(blockIDs)
	blocks = fromSQLBlocks(&sqlBlocks, "", 36)
	if 1 > len(blocks) {
		blocks = []*Block{}
		return
	}

	for i, b := range blocks {
		if nil == b {
			blocks[i] = &Block{
				ID:      blockIDs[i],
				Content: Conf.Language(180),
			}

			continue
		}

		b.RiffCardID = cards[i].ID()
	}
	return
}

// reviewCardCache <cardID, card> 用于复习时缓存卡片，以便支持撤销。
var reviewCardCache = map[string]riff.Card{}

func ReviewFlashcard(deckID, cardID string, rating riff.Rating) (err error) {
	deckLock.Lock()
	defer deckLock.Unlock()

	if syncingStorages {
		err = errors.New(Conf.Language(81))
		return
	}

	deck := Decks[deckID]
	card := deck.GetCard(cardID)
	if nil == card {
		return
	}

	if cachedCard := reviewCardCache[cardID]; nil != cachedCard {
		// 命中缓存说明这张卡片已经复习过了，这次调用复习是撤销后再次复习
		// 将缓存的卡片重新覆盖回卡包中，以恢复最开始复习前的状态
		deck.SetCard(cachedCard)
	} else {
		// 首次复习该卡片，将卡片缓存以便后续支持撤销后再次复习
		reviewCardCache[cardID] = card
	}

	deck.Review(cardID, rating)
	err = deck.Save()
	if nil != err {
		logging.LogErrorf("save deck [%s] failed: %s", deckID, err)
		return
	}

	dueCards := getDueFlashcards(deckID)
	if 1 > len(dueCards) {
		// 该卡包中没有待复习的卡片了，说明最后一张卡片已经复习完了，清空撤销缓存
		reviewCardCache = map[string]riff.Card{}
	}
	return
}

type Flashcard struct {
	DeckID   string                 `json:"deckID"`
	CardID   string                 `json:"cardID"`
	BlockID  string                 `json:"blockID"`
	NextDues map[riff.Rating]string `json:"nextDues"`
}

func newFlashcard(card riff.Card, blockID, deckID string, now time.Time) *Flashcard {
	nextDues := map[riff.Rating]string{}
	for rating, due := range card.NextDues() {
		nextDues[rating] = strings.TrimSpace(util.HumanizeRelTime(due, now, Conf.Lang))
	}

	return &Flashcard{
		DeckID:   deckID,
		CardID:   card.ID(),
		BlockID:  blockID,
		NextDues: nextDues,
	}
}

func GetNotebookDueFlashcards(boxID string) (ret []*Flashcard, err error) {
	deckLock.Lock()
	defer deckLock.Unlock()

	if syncingStorages {
		err = errors.New(Conf.Language(81))
		return
	}

	entries, err := os.ReadDir(filepath.Join(util.DataDir, boxID))
	if nil != err {
		logging.LogErrorf("read dir failed: %s", err)
		return
	}

	var rootIDs []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		if !strings.HasSuffix(entry.Name(), ".sy") {
			continue
		}

		rootIDs = append(rootIDs, strings.TrimSuffix(entry.Name(), ".sy"))
	}

	treeBlockIDs := map[string]bool{}
	for _, rootID := range rootIDs {
		blockIDs := getTreeSubTreeChildBlocks(rootID)
		for blockID, _ := range blockIDs {
			treeBlockIDs[blockID] = true
		}
	}

	deck := Decks[builtinDeckID]
	if nil == deck {
		logging.LogWarnf("builtin deck not found")
		return
	}

	cards := deck.Dues()
	now := time.Now()
	for _, card := range cards {
		blockID := card.BlockID()
		if !treeBlockIDs[blockID] {
			continue
		}

		ret = append(ret, newFlashcard(card, blockID, builtinDeckID, now))
	}
	if 1 > len(ret) {
		ret = []*Flashcard{}
	}
	return
}

func GetTreeDueFlashcards(rootID string) (ret []*Flashcard, err error) {
	deckLock.Lock()
	defer deckLock.Unlock()

	if syncingStorages {
		err = errors.New(Conf.Language(81))
		return
	}

	deck := Decks[builtinDeckID]
	if nil == deck {
		return
	}

	treeBlockIDs := getTreeSubTreeChildBlocks(rootID)
	cards := deck.Dues()
	now := time.Now()
	for _, card := range cards {
		blockID := card.BlockID()
		if !treeBlockIDs[blockID] {
			continue
		}

		ret = append(ret, newFlashcard(card, blockID, builtinDeckID, now))
	}
	if 1 > len(ret) {
		ret = []*Flashcard{}
	}
	return
}

func getTreeSubTreeChildBlocks(rootID string) (treeBlockIDs map[string]bool) {
	treeBlockIDs = map[string]bool{}

	tree, err := loadTreeByBlockID(rootID)
	if nil != err {
		return
	}

	trees := []*parse.Tree{tree}
	box := Conf.Box(tree.Box)
	luteEngine := util.NewLute()
	files := box.ListFiles(tree.Path)
	for _, subFile := range files {
		if !strings.HasSuffix(subFile.path, ".sy") {
			continue
		}

		subTree, loadErr := filesys.LoadTree(box.ID, subFile.path, luteEngine)
		if nil != loadErr {
			continue
		}

		trees = append(trees, subTree)
	}

	for _, t := range trees {
		ast.Walk(t.Root, func(n *ast.Node, entering bool) ast.WalkStatus {
			if !entering || !n.IsBlock() {
				return ast.WalkContinue
			}

			treeBlockIDs[n.ID] = true
			return ast.WalkContinue
		})
	}
	return
}

func GetDueFlashcards(deckID string) (ret []*Flashcard, err error) {
	deckLock.Lock()
	defer deckLock.Unlock()

	if syncingStorages {
		err = errors.New(Conf.Language(81))
		return
	}

	if "" == deckID {
		ret = getAllDueFlashcards()
		return
	}

	ret = getDueFlashcards(deckID)
	return
}

func getDueFlashcards(deckID string) (ret []*Flashcard) {
	deck := Decks[deckID]
	if nil == deck {
		logging.LogWarnf("deck not found [%s]", deckID)
		return
	}

	cards := deck.Dues()
	now := time.Now()
	for _, card := range cards {
		blockID := card.BlockID()

		if nil == treenode.GetBlockTree(blockID) {
			continue
		}

		ret = append(ret, newFlashcard(card, blockID, deckID, now))
	}
	if 1 > len(ret) {
		ret = []*Flashcard{}
	}
	return
}

func getAllDueFlashcards() (ret []*Flashcard) {
	now := time.Now()
	for _, deck := range Decks {
		cards := deck.Dues()
		for _, card := range cards {
			blockID := card.BlockID()
			if nil == treenode.GetBlockTree(blockID) {
				continue
			}

			ret = append(ret, newFlashcard(card, blockID, deck.ID, now))
		}
	}
	if 1 > len(ret) {
		ret = []*Flashcard{}
	}
	return
}

func RemoveFlashcardsByCardIDs(deckID string, cardIDs []string) (err error) {
	deckLock.Lock()
	defer deckLock.Unlock()

	if syncingStorages {
		err = errors.New(Conf.Language(81))
		return
	}

	var needRemoveDeckAttrBlockIDs []string
	if "" == deckID {
		// 在 All 卡包中移除
		var affectedBlockIDs []string
		for _, deck := range Decks {
			changed := false
			for _, cardID := range cardIDs {
				card := deck.GetCard(cardID)
				if nil == card {
					continue
				}

				affectedBlockIDs = append(affectedBlockIDs, card.BlockID())
				deck.RemoveCard(cardID)
				changed = true
			}

			if changed {
				if err = deck.Save(); nil != err {
					return
				}
			}

			// 检查刚刚移除的卡片关联的块是否还存在更多关联的卡片
			affectedBlockIDs = gulu.Str.RemoveDuplicatedElem(affectedBlockIDs)
			for _, blockID := range affectedBlockIDs {
				moreRelatedCards := deck.GetCardsByBlockID(blockID)
				if 1 > len(moreRelatedCards) {
					needRemoveDeckAttrBlockIDs = append(needRemoveDeckAttrBlockIDs, blockID)
				}
			}
		}
	} else {
		// 在指定卡包中移除
		deck := Decks[deckID]
		if nil == deck {
			return
		}

		var affectedBlockIDs []string
		for _, cardID := range cardIDs {
			card := deck.GetCard(cardID)
			if nil == card {
				continue
			}

			affectedBlockIDs = append(affectedBlockIDs, card.BlockID())
			deck.RemoveCard(cardID)
			if err = deck.Save(); nil != err {
				return
			}
		}

		// 检查刚刚移除的卡片关联的块是否还存在更多关联的卡片
		affectedBlockIDs = gulu.Str.RemoveDuplicatedElem(affectedBlockIDs)
		for _, blockID := range affectedBlockIDs {
			moreRelatedCards := deck.GetCardsByBlockID(blockID)
			if 1 > len(moreRelatedCards) {
				needRemoveDeckAttrBlockIDs = append(needRemoveDeckAttrBlockIDs, blockID)
			}
		}
	}

	if err = removeBlocksDeckAttr(needRemoveDeckAttrBlockIDs, deckID); nil != err {
		return
	}
	return
}

func RemoveFlashcardsByBlockIDs(deckID string, blockIDs []string) (err error) {
	deckLock.Lock()
	defer deckLock.Unlock()

	if syncingStorages {
		err = errors.New(Conf.Language(81))
		return
	}

	if err = removeBlocksDeckAttr(blockIDs, deckID); nil != err {
		return
	}

	if "" == deckID { // 支持在 All 卡包中移除闪卡 https://github.com/siyuan-note/siyuan/issues/7425
		for _, deck := range Decks {
			removeFlashcardsByBlockIDs(blockIDs, deck)
		}
	} else {
		removeFlashcardsByBlockIDs(blockIDs, Decks[deckID])
	}
	return
}

func removeBlocksDeckAttr(blockIDs []string, deckID string) (err error) {
	var rootIDs []string
	blockRoots := map[string]string{}
	for _, blockID := range blockIDs {
		bt := treenode.GetBlockTree(blockID)
		if nil == bt {
			continue
		}

		rootIDs = append(rootIDs, bt.RootID)
		blockRoots[blockID] = bt.RootID
	}
	rootIDs = gulu.Str.RemoveDuplicatedElem(rootIDs)

	trees := map[string]*parse.Tree{}
	for _, blockID := range blockIDs {
		rootID := blockRoots[blockID]

		tree := trees[rootID]
		if nil == tree {
			tree, _ = loadTreeByBlockID(blockID)
		}
		if nil == tree {
			continue
		}
		trees[rootID] = tree

		node := treenode.GetNodeInTree(tree, blockID)
		if nil == node {
			continue
		}

		oldAttrs := parse.IAL2Map(node.KramdownIAL)

		deckAttrs := node.IALAttr("custom-riff-decks")
		var deckIDs []string
		if "" != deckID {
			availableDeckIDs := getDeckIDs()
			for _, dID := range strings.Split(deckAttrs, ",") {
				if dID != deckID && gulu.Str.Contains(dID, availableDeckIDs) {
					deckIDs = append(deckIDs, dID)
				}
			}
		}

		deckIDs = gulu.Str.RemoveDuplicatedElem(deckIDs)
		val := strings.Join(deckIDs, ",")
		val = strings.TrimPrefix(val, ",")
		val = strings.TrimSuffix(val, ",")
		if "" == val {
			node.RemoveIALAttr("custom-riff-decks")
		} else {
			node.SetIALAttr("custom-riff-decks", val)
		}

		if err = indexWriteJSONQueue(tree); nil != err {
			return
		}

		cache.PutBlockIAL(blockID, parse.IAL2Map(node.KramdownIAL))
		pushBroadcastAttrTransactions(oldAttrs, node)
	}

	return
}

func removeFlashcardsByBlockIDs(blockIDs []string, deck *riff.Deck) {
	if nil == deck {
		logging.LogErrorf("deck is nil")
		return
	}

	cards := deck.GetCardsByBlockIDs(blockIDs)
	for _, card := range cards {
		deck.RemoveCard(card.ID())
	}
	err := deck.Save()
	if nil != err {
		logging.LogErrorf("save deck [%s] failed: %s", deck.ID, err)
	}
}

func AddFlashcards(deckID string, blockIDs []string) (err error) {
	deckLock.Lock()
	defer deckLock.Unlock()

	if syncingStorages {
		err = errors.New(Conf.Language(81))
		return
	}

	blockRoots := map[string]string{}
	for _, blockID := range blockIDs {
		bt := treenode.GetBlockTree(blockID)
		if nil == bt {
			continue
		}

		blockRoots[blockID] = bt.RootID
	}

	trees := map[string]*parse.Tree{}
	for _, blockID := range blockIDs {
		rootID := blockRoots[blockID]

		tree := trees[rootID]
		if nil == tree {
			tree, _ = loadTreeByBlockID(blockID)
		}
		if nil == tree {
			continue
		}
		trees[rootID] = tree

		node := treenode.GetNodeInTree(tree, blockID)
		if nil == node {
			continue
		}

		oldAttrs := parse.IAL2Map(node.KramdownIAL)

		deckAttrs := node.IALAttr("custom-riff-decks")
		deckIDs := strings.Split(deckAttrs, ",")
		deckIDs = append(deckIDs, deckID)
		deckIDs = gulu.Str.RemoveDuplicatedElem(deckIDs)
		val := strings.Join(deckIDs, ",")
		val = strings.TrimPrefix(val, ",")
		val = strings.TrimSuffix(val, ",")
		node.SetIALAttr("custom-riff-decks", val)

		if err = indexWriteJSONQueue(tree); nil != err {
			return
		}

		cache.PutBlockIAL(blockID, parse.IAL2Map(node.KramdownIAL))
		pushBroadcastAttrTransactions(oldAttrs, node)
	}

	deck := Decks[deckID]
	if nil == deck {
		logging.LogWarnf("deck [%s] not found", deckID)
		return
	}

	for _, blockID := range blockIDs {
		cards := deck.GetCardsByBlockID(blockID)
		if 0 < len(cards) {
			// 一个块只能添加生成一张闪卡 https://github.com/siyuan-note/siyuan/issues/7476
			continue
		}

		cardID := ast.NewNodeID()
		deck.AddCard(cardID, blockID)
	}
	err = deck.Save()
	if nil != err {
		logging.LogErrorf("save deck [%s] failed: %s", deckID, err)
		return
	}
	return
}

func LoadFlashcards() {
	riffSavePath := getRiffDir()
	if err := os.MkdirAll(riffSavePath, 0755); nil != err {
		logging.LogErrorf("create riff dir [%s] failed: %s", riffSavePath, err)
		return
	}

	Decks = map[string]*riff.Deck{}

	entries, err := os.ReadDir(riffSavePath)
	if nil != err {
		logging.LogErrorf("read riff dir failed: %s", err)
		return
	}
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasSuffix(name, ".deck") {
			deckID := strings.TrimSuffix(name, ".deck")
			deck, loadErr := riff.LoadDeck(riffSavePath, deckID)
			if nil != loadErr {
				logging.LogErrorf("load deck [%s] failed: %s", name, loadErr)
				continue
			}

			if 0 == deck.Created {
				deck.Created = time.Now().Unix()
			}
			if 0 == deck.Updated {
				deck.Updated = deck.Created
			}

			Decks[deckID] = deck
		}
	}

	if 1 > len(Decks) {
		deck, createErr := createDeck("Default Deck")
		if nil == createErr {
			Decks[deck.ID] = deck
		}
	}

	// 支持基于文档复习闪卡 https://github.com/siyuan-note/siyuan/issues/7057
	foudBuiltinDeck := false
	for _, deck := range Decks {
		if builtinDeckID == deck.ID {
			foudBuiltinDeck = true
			break
		}
	}
	if !foudBuiltinDeck {
		deck, createErr := createDeck0("Built-in Deck", builtinDeckID)
		if nil == createErr {
			Decks[deck.ID] = deck
		}
	}
}

const builtinDeckID = "20230218211946-2kw8jgx"

func RenameDeck(deckID, name string) (err error) {
	deckLock.Lock()
	defer deckLock.Unlock()

	if syncingStorages {
		err = errors.New(Conf.Language(81))
		return
	}

	deck := Decks[deckID]
	deck.Name = name
	err = deck.Save()
	if nil != err {
		logging.LogErrorf("save deck [%s] failed: %s", deckID, err)
		return
	}
	return
}

func RemoveDeck(deckID string) (err error) {
	deckLock.Lock()
	defer deckLock.Unlock()

	if syncingStorages {
		err = errors.New(Conf.Language(81))
		return
	}

	riffSavePath := getRiffDir()
	deckPath := filepath.Join(riffSavePath, deckID+".deck")
	if gulu.File.IsExist(deckPath) {
		if err = os.Remove(deckPath); nil != err {
			return
		}
	}

	cardsPath := filepath.Join(riffSavePath, deckID+".cards")
	if gulu.File.IsExist(cardsPath) {
		if err = os.Remove(cardsPath); nil != err {
			return
		}
	}

	LoadFlashcards()
	return
}

func CreateDeck(name string) (deck *riff.Deck, err error) {
	deckLock.Lock()
	defer deckLock.Unlock()
	return createDeck(name)
}

func createDeck(name string) (deck *riff.Deck, err error) {
	if syncingStorages {
		err = errors.New(Conf.Language(81))
		return
	}

	deckID := ast.NewNodeID()
	deck, err = createDeck0(name, deckID)
	return
}

func createDeck0(name string, deckID string) (deck *riff.Deck, err error) {
	riffSavePath := getRiffDir()
	deck, err = riff.LoadDeck(riffSavePath, deckID)
	if nil != err {
		logging.LogErrorf("load deck [%s] failed: %s", deckID, err)
		return
	}
	deck.Name = name
	Decks[deckID] = deck
	err = deck.Save()
	if nil != err {
		logging.LogErrorf("save deck [%s] failed: %s", deckID, err)
		return
	}
	return
}

func GetDecks() (decks []*riff.Deck) {
	deckLock.Lock()
	defer deckLock.Unlock()

	for _, deck := range Decks {
		if deck.ID == builtinDeckID {
			continue
		}

		decks = append(decks, deck)
	}
	if 1 > len(decks) {
		decks = []*riff.Deck{}
	}

	sort.Slice(decks, func(i, j int) bool {
		return decks[i].Updated > decks[j].Updated
	})
	return
}

func getRiffDir() string {
	return filepath.Join(util.DataDir, "storage", "riff")
}

func getDeckIDs() (deckIDs []string) {
	for deckID := range Decks {
		deckIDs = append(deckIDs, deckID)
	}
	return
}
