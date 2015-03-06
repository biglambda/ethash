package ethash

/*
#cgo CFLAGS: -std=gnu99 -Wall
#include "src/libethash/util.c"
#include "src/libethash/internal.c"
#include "src/libethash/sha3.c"
*/
import "C"

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io/ioutil"
	"log"
	"math/big"
	"math/rand"
	"os"
	"path"
	"sync"
	"time"
	"unsafe"

	"github.com/ethereum/go-ethereum/ethutil"
	"github.com/ethereum/go-ethereum/logger"
	"github.com/ethereum/go-ethereum/pow"
)

var tt256 = new(big.Int).Exp(big.NewInt(2), big.NewInt(256), big.NewInt(0))

var powlogger = logger.NewLogger("POW")

type DAG struct {
	SeedBlockNum uint64
	dag          unsafe.Pointer // full GB of memory for dag
}

type ParamsAndCache struct {
	params       *C.ethash_params
	cache        *C.ethash_cache
	SeedBlockNum uint64
}

type Ethash struct {
	turbo          bool
	HashRate       int64
	chainManager   pow.ChainManager
	dag            *DAG
	paramsAndCache *ParamsAndCache
	nextdag        unsafe.Pointer
	ret            *C.ethash_return_value
	dagMutex       *sync.Mutex
	cacheMutex     *sync.Mutex
}

func parseNonce(nonce []byte) (uint64, error) {
	nonceBuf := bytes.NewBuffer(nonce)
	nonceInt, err := binary.ReadUvarint(nonceBuf)
	if err != nil {
		return 0, err
	}
	return nonceInt, nil
}

const epochLength uint64 = 30000

func GetSeedBlockNum(blockNum uint64) uint64 {
	var seedBlockNum uint64 = 0
	if blockNum > epochLength {
		seedBlockNum = ((blockNum - 1) / epochLength) * epochLength
	}
	return seedBlockNum
}

func makeParamsAndCache(chainManager pow.ChainManager, blockNum uint64) *ParamsAndCache {
	seedBlockNum := GetSeedBlockNum(blockNum)
	paramsAndCache := &ParamsAndCache{
		params:       new(C.ethash_params),
		cache:        new(C.ethash_cache),
		SeedBlockNum: seedBlockNum,
	}
	C.ethash_params_init(paramsAndCache.params, C.uint32_t(seedBlockNum))
	paramsAndCache.cache.mem = C.malloc(paramsAndCache.params.cache_size)
	seedHash := chainManager.GetBlockByNumber(seedBlockNum).SeedHash()

	log.Println("Making Cache")
	start := time.Now()
	C.ethash_mkcache(paramsAndCache.cache, paramsAndCache.params, (*C.uint8_t)(unsafe.Pointer(&seedHash[0])))
	log.Println("Took:", time.Since(start))

	return paramsAndCache
}

func (pow *Ethash) updateCache() {
	pow.cacheMutex.Lock()
	seedNum := GetSeedBlockNum(pow.chainManager.CurrentBlock().NumberU64())
	if pow.paramsAndCache.SeedBlockNum != seedNum {
		pow.paramsAndCache = makeParamsAndCache(pow.chainManager, pow.chainManager.CurrentBlock().NumberU64())
	}
	pow.cacheMutex.Unlock()
}

func makeDAG(p *ParamsAndCache) *DAG {
	d := &DAG{
		dag:          C.malloc(p.params.full_size),
		SeedBlockNum: p.SeedBlockNum,
	}
	C.ethash_compute_full_data(d.dag, p.params, p.cache)
	return d
}

func (pow *Ethash) writeDagToDisk(dag *DAG, seedNum uint64) *os.File {
	data := C.GoBytes(unsafe.Pointer(dag.dag), C.int(pow.paramsAndCache.params.full_size))
	file, err := os.Create("/tmp/dag")
	if err != nil {
		panic(err)
	}

	num := make([]byte, 8)
	binary.BigEndian.PutUint64(num, seedNum)

	file.Write(num)
	file.Write(data)

	return file
}

func (pow *Ethash) updateDAG() {
	pow.cacheMutex.Lock()
	pow.dagMutex.Lock()

	seedNum := GetSeedBlockNum(pow.chainManager.CurrentBlock().NumberU64())
	if pow.dag == nil || pow.dag.SeedBlockNum != seedNum {
		if pow.dag != nil && pow.dag.dag != nil {
			C.free(pow.dag.dag)
		}

		path := path.Join("/", "tmp", "dag")
		pow.dag = nil
		log.Println("Generating dag Dag")
		start := time.Now()

		file, err := os.Open(path)
		if err != nil {
			log.Printf("No dag found in '%s'. Generating new dago(takes a while)...")
			pow.dag = makeDAG(pow.paramsAndCache)
			file = pow.writeDagToDisk(pow.dag, seedNum)
		} else {
			data, err := ioutil.ReadAll(file)
			if err != nil {
				panic(err)
			}

			num := binary.BigEndian.Uint64(data[0:8])
			if num < seedNum {
				log.Printf("Old found. Generating new dag (takes a while)...")
				pow.dag = makeDAG(pow.paramsAndCache)
				file = pow.writeDagToDisk(pow.dag, seedNum)
			} else {
				data = data[8:]
				pow.dag = &DAG{
					dag:          unsafe.Pointer(&data[0]),
					SeedBlockNum: pow.paramsAndCache.SeedBlockNum,
				}
				//fmt.Printf("SHA DAG: %x\n", crypto.Sha3(data))
			}
		}
		log.Println("Took:", time.Since(start))

		file.Close()
	}

	pow.dagMutex.Unlock()
	pow.cacheMutex.Unlock()
}

func New(chainManager pow.ChainManager) *Ethash {
	return &Ethash{
		turbo:          true,
		paramsAndCache: makeParamsAndCache(chainManager, chainManager.CurrentBlock().NumberU64()),
		chainManager:   chainManager,
		dag:            nil,
		ret:            new(C.ethash_return_value),
		cacheMutex:     new(sync.Mutex),
		dagMutex:       new(sync.Mutex),
	}
}

func (pow *Ethash) DAGSize() uint64 {
	return uint64(pow.paramsAndCache.params.full_size)
}

func (pow *Ethash) CacheSize() uint64 {
	return uint64(pow.paramsAndCache.params.cache_size)
}

func (pow *Ethash) GetSeedHash(blockNum uint64) []byte {
	seednum := GetSeedBlockNum(blockNum)
	return pow.chainManager.GetBlockByNumber(seednum).SeedHash()
}

func (pow *Ethash) Stop() {
	pow.cacheMutex.Lock()
	pow.dagMutex.Lock()
	if pow.paramsAndCache.cache != nil {
		C.free(pow.paramsAndCache.cache.mem)
	}
	if pow.dag.dag != nil {
		C.free(pow.dag.dag)
	}
	pow.dagMutex.Unlock()
	pow.cacheMutex.Unlock()
}

func (pow *Ethash) Search(block pow.Block, stop <-chan struct{}) (uint64, []byte, []byte) {
	pow.updateDAG()

	// Not very elegant, multiple mining instances are not supported
	pow.dagMutex.Lock()
	pow.cacheMutex.Lock()
	defer pow.cacheMutex.Unlock()
	defer pow.dagMutex.Unlock()

	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	miningHash := block.HashNoNonce()
	diff := block.Difficulty()

	i := int64(0)
	start := time.Now().UnixNano()
	starti := start

	nonce := uint64(r.Int63())
	cMiningHash := (*C.uint8_t)(unsafe.Pointer(&miningHash[0]))
	target := new(big.Int).Div(tt256, diff)

	for {
		select {
		case <-stop:
			powlogger.Infoln("Breaking from mining")
			pow.HashRate = 0
			return 0, nil, nil
		default:
			i++

			elapsed := time.Now().UnixNano() - start
			hashes := ((float64(1e9) / float64(elapsed)) * float64(i-starti)) / 1000
			pow.HashRate = int64(hashes)

			C.ethash_full(pow.ret, pow.dag.dag, pow.paramsAndCache.params, cMiningHash, C.uint64_t(nonce))
			result := ethutil.Bytes2Big(C.GoBytes(unsafe.Pointer(&pow.ret.result[0]), C.int(32)))

			if result.Cmp(target) <= 0 {
				mixDigest := C.GoBytes(unsafe.Pointer(&pow.ret.mix_hash[0]), C.int(32))

				return nonce, mixDigest, pow.GetSeedHash(block.NumberU64())

			}

			nonce += 1
		}

		if !pow.turbo {
			time.Sleep(20 * time.Microsecond)
		}
	}
}

func (pow *Ethash) Verify(block pow.Block) bool {
	// Make sure the SeedHash is set correctly
	if bytes.Compare(block.SeedHash(), pow.GetSeedHash(block.NumberU64())) != 0 {
		return false
	}

	return pow.verify(block.HashNoNonce(), block.MixDigest(), block.Difficulty(), block.NumberU64(), block.Nonce())
}

func (pow *Ethash) verify(hash []byte, mixDigest []byte, difficulty *big.Int, blockNum uint64, nonce uint64) bool {
	fmt.Printf("%x\n%d\n%x\n%x\n", hash, nonce, mixDigest, difficulty.Bytes())
	// First check: make sure header, mixDigest, nonce are correct without hitting the DAG
	// This is to prevent DOS attacks
	chash := (*C.uint8_t)(unsafe.Pointer(&hash[0]))
	cnonce := C.uint64_t(nonce)
	target := new(big.Int).Div(tt256, difficulty)

	var pAc *ParamsAndCache
	// If its an old block (doesn't use the current cache)
	// get the cache for it but don't update (so we don't need the mutex)
	// Otherwise, it's the current block or a future.
	// If current, updateCache will do nothing.
	if GetSeedBlockNum(blockNum) < pow.paramsAndCache.SeedBlockNum {
		pAc = makeParamsAndCache(pow.chainManager, blockNum)
	} else {
		pow.updateCache()
		pow.cacheMutex.Lock()
		defer pow.cacheMutex.Unlock()
		pAc = pow.paramsAndCache
	}

	C.ethash_light(pow.ret, pAc.cache, pAc.params, chash, cnonce)

	result := ethutil.Bytes2Big(C.GoBytes(unsafe.Pointer(&pow.ret.result[0]), C.int(32)))
	return result.Cmp(target) <= 0
}

func (pow *Ethash) GetHashrate() int64 {
	return pow.HashRate
}

func (pow *Ethash) Turbo(on bool) {
	pow.turbo = on
}

func (pow *Ethash) FullHash(nonce uint64, miningHash []byte) []byte {
	pow.updateDAG()
	pow.dagMutex.Lock()
	defer pow.dagMutex.Unlock()
	cMiningHash := (*C.uint8_t)(unsafe.Pointer(&miningHash[0]))
	cnonce := C.uint64_t(nonce)
	// pow.hash is the output/return of ethash_full
	C.ethash_full(pow.ret, pow.dag.dag, pow.paramsAndCache.params, cMiningHash, cnonce)
	ghash_full := C.GoBytes(unsafe.Pointer(&pow.ret.result), 32)
	return ghash_full
}

func (pow *Ethash) LightHash(nonce uint64, miningHash []byte) []byte {
	cMiningHash := (*C.uint8_t)(unsafe.Pointer(&miningHash[0]))
	cnonce := C.uint64_t(nonce)
	C.ethash_light(pow.ret, pow.paramsAndCache.cache, pow.paramsAndCache.params, cMiningHash, cnonce)
	ghash_light := C.GoBytes(unsafe.Pointer(&pow.ret.result), 32)
	return ghash_light
}
