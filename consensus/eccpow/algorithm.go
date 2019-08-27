package eccpow

import (
	"crypto/sha256"
	"fmt"
	"math"
	"math/big"
	"math/rand"
	"strconv"
	"sync"
	"time"

	"github.com/Onther-Tech/go-ethereum/crypto"

	"github.com/Onther-Tech/go-ethereum/common"
	"github.com/Onther-Tech/go-ethereum/consensus"
	"github.com/Onther-Tech/go-ethereum/core/types"
	"github.com/Onther-Tech/go-ethereum/metrics"
	"github.com/Onther-Tech/go-ethereum/rpc"
)

type ECC struct {
	shared *ECC

	// Mining related fields
	rand     *rand.Rand    // Properly seeded random source for nonces
	threads  int           // Number of threads to mine on if mining
	update   chan struct{} // Notification channel to update mining parameters
	hashrate metrics.Meter // Meter tracking the average hashrate

	// Remote sealer related fields
	workCh       chan *sealTask   // Notification channel to push new work and relative result channel to remote sealer
	fetchWorkCh  chan *sealWork   // Channel used for remote sealer to fetch mining work
	submitWorkCh chan *mineResult // Channel used for remote sealer to submit their mining result
	fetchRateCh  chan chan uint64 // Channel used to gather submitted hash rate for local or remote sealer.
	submitRateCh chan *hashrate   // Channel used for remote sealer to submit their mining hashrate

	lock      sync.Mutex      // Ensures thread safety for the in-memory caches and mining fields
	closeOnce sync.Once       // Ensures exit channel will not be closed twice.
	exitCh    chan chan error // Notification channel to exiting backend threads

}

// sealTask wraps a seal block with relative result channel for remote sealer thread.
type sealTask struct {
	block   *types.Block
	results chan<- *types.Block
}

// mineResult wraps the pow solution parameters for the specified block.
type mineResult struct {
	nonce     types.BlockNonce
	mixDigest common.Hash
	hash      common.Hash

	errc chan error
}

// hashrate wraps the hash rate submitted by the remote sealer.
type hashrate struct {
	id   common.Hash
	ping time.Time
	rate uint64

	done chan struct{}
}

// sealWork wraps a seal work package for remote sealer.
type sealWork struct {
	errc chan error
	res  chan [4]string
}

// hasher is a repetitive hasher allowing the same hash data structures to be
// reused between hash runs instead of requiring new ones to be created.
//var hasher func(dest []byte, data []byte)

const (
	BigInfinity = 1000000.0
	Inf         = 64.0
	MaxNonce    = 1<<32 - 1

	// These parameters are only used for the decoding function.
	maxIter  = 20   // The maximum number of iteration in the decoder
	crossErr = 0.01 // A transisient error probability. This is also fixed as a small value
)

var two256 = new(big.Int).Exp(big.NewInt(2), big.NewInt(256), big.NewInt(0))

//Parameters for matrix and seed
type Parameters struct {
	n    int
	m    int
	wc   int
	wr   int
	seed int
}

//const cross_err = 0.01

//type (
//	intMatrix   [][]int
//	floatMatrix [][]float64
//)

//SetDifficultyUsingLevel set matrix parameters
//Only 4 parameters are valied 0 : Very easy, 1 : Easy, 2 : Medium, 3 : hard
func SetDifficultyUsingLevel(level int) Parameters {
	//level 4 is max level
	if level > 4 {
		level = 4
	}

	parameters := Parameters{}
	if level == 0 {
		parameters.n = 16
		parameters.wc = 3
		parameters.wr = 4
	} else if level == 1 {
		parameters.n = 32
		parameters.wc = 3
		parameters.wr = 4
	} else if level == 2 {
		parameters.n = 64
		parameters.wc = 3
		parameters.wr = 4
	} else if level == 3 {
		parameters.n = 128
		parameters.wc = 3
		parameters.wr = 4
	}
	parameters.m = int(parameters.n * parameters.wc / parameters.wr)

	return parameters
}

//GenerateSeed generate seed using previous hash vector
func GenerateSeed(phv []byte) int {
	sum := 0
	for i := 0; i < len(phv); i++ {
		sum += int(phv[i])
	}
	return sum
}

//Decoding carry out LDPC decoding. It returns hashVector and outputWord
func Decoding(parameters Parameters,
	hashVector []int,
	H, rowInCol, colInRow [][]int,
) ([]int, []int, [][]float64) {
	var temp3, tempSign, sign, magnitude float64

	outputWord := make([]int, parameters.n)
	LRqtl := make([][]float64, parameters.n)
	LRrtl := make([][]float64, parameters.n)
	LRft := make([]float64, parameters.n)

	for i := 0; i < parameters.n; i++ {
		LRqtl[i] = make([]float64, parameters.m)
		LRrtl[i] = make([]float64, parameters.m)
		LRft[i] = math.Log((1-crossErr)/crossErr) * float64((hashVector[i]*2 - 1))
	}
	LRpt := make([]float64, parameters.n)

	var i, k, l, m, ind, t, mp int
	for ind = 1; ind <= maxIter; ind++ {
		for t = 0; t < parameters.n; t++ {
			for m = 0; m < parameters.wc; m++ {
				temp3 = 0
				for mp = 0; mp < parameters.wc; mp++ {
					if mp != m {
						temp3 = infinityTest(temp3 + LRrtl[t][rowInCol[mp][t]])
					}
				}
				LRqtl[t][rowInCol[m][t]] = infinityTest(LRft[t] + temp3)
			}
		}
		for k = 0; k < m; k++ {
			for l = 0; l < parameters.wr; l++ {
				temp3 = 0.0
				sign = 1
				for m = 0; m < parameters.wr; m++ {
					if m != l {
						temp3 = temp3 + funcF(math.Abs(LRqtl[colInRow[m][k]][k]))
						if LRqtl[colInRow[m][k]][k] > 0.0 {
							tempSign = 1.0
						} else {
							tempSign = -1.0
						}
						sign = sign * tempSign
					}

				}
				magnitude = funcF(temp3)
				LRrtl[colInRow[l][k]][k] = infinityTest(sign * magnitude)
			}
		}
		for m = 0; m < parameters.n; m++ {
			LRpt[m] = infinityTest(LRft[m])
			for k = 0; k < parameters.wc; k++ {
				LRpt[m] += LRrtl[m][rowInCol[k][m]]
				LRpt[m] = infinityTest(LRpt[m])
			}
		}
	}
	for i = 0; i < parameters.n; i++ {
		if LRpt[i] >= 0 {
			outputWord[i] = 1
		} else {
			outputWord[i] = 0
		}
	}

	return hashVector, outputWord, LRrtl
}

//GenerateH generate H matrix using parameters
//GenerateH Cannot be sure rand is same with original implementation of C++
func GenerateH(parameters Parameters) [][]int {
	var H [][]int
	var hSeed int64
	hSeed = int64(parameters.seed)

	var colOrder []int
	/*
		if H == nil {
			return false
		}
	*/
	k := parameters.m / parameters.wc
	H = make([][]int, parameters.m)
	for i := range H {
		H[i] = make([]int, parameters.n)
	}

	for i := 0; i < k; i++ {
		for j := i * parameters.wr; j < (i+1)*parameters.wr; j++ {
			H[i][j] = 1
		}
	}

	for i := 1; i < parameters.wc; i++ {
		colOrder = nil
		for j := 0; j < parameters.n; j++ {
			colOrder = append(colOrder, j)
		}

		rand.Seed(hSeed)
		rand.Shuffle(len(colOrder), func(i, j int) {
			colOrder[i], colOrder[j] = colOrder[j], colOrder[i]
		})
		hSeed--

		for j := 0; j < parameters.n; j++ {
			index := (colOrder[j]/parameters.wr + k*i)
			H[index][j] = 1
		}
	}

	return H
}

//GenerateQ generate colInRow and rowInCol matrix using H matrix
func GenerateQ(parameters Parameters, H [][]int) ([][]int, [][]int) {
	colInRow := make([][]int, parameters.wr)
	for i := 0; i < parameters.wr; i++ {
		colInRow[i] = make([]int, parameters.m)
	}

	rowInCol := make([][]int, parameters.wc)
	for i := 0; i < parameters.wc; i++ {
		rowInCol[i] = make([]int, parameters.n)
	}

	rowIndex := 0
	colIndex := 0

	for i := 0; i < parameters.m; i++ {
		for j := 0; j < parameters.n; j++ {
			if H[i][j] == 1 {
				colInRow[colIndex%parameters.wr][i] = j
				colIndex++

				rowInCol[rowIndex/parameters.n][j] = i
				rowIndex++
			}
		}
	}

	return colInRow, rowInCol
}

//GenerateHv generate hashvector
//It needs to compare with origin C++ implementation Especially when sha256 function is used
func GenerateHv(parameters Parameters, headerWithNonce []byte) []int {
	//inputSize := len(headerWithNonce)
	var tmpHashVector [32]byte //32bytes => 256 bits
	hashVector := make([]int, parameters.n)

	if parameters.n <= 256 {
		tmpHashVector = sha256.Sum256(headerWithNonce)
	} else {
		/*
			This section is for a case in which the size of a hash vector is larger than 256.
			This section will be implemented soon.
		*/
	}

	/*
		transform the constructed hexadecimal array into an binary array
		ex) FE01 => 11111110000 0001
	*/
	for i := 0; i < parameters.n/8; i++ {
		decimal := int(tmpHashVector[i])
		for j := 7; j >= 0; j-- {
			hashVector[j+8*(i)] = decimal % 2
			decimal /= 2
		}
	}

	//outputWord := hashVector[:parameters.n]
	return hashVector
}

//MakeDecision check outputWord is valid or not using colInRow
func MakeDecision(parameters Parameters, colInRow [][]int, outputWord []int) bool {
	for i := 0; i < parameters.m; i++ {
		sum := 0
		for j := 0; j < parameters.wr; j++ {
			//	fmt.Printf("i : %d, j : %d, m : %d, wr : %d \n", i, j, m, wr)
			sum = sum + outputWord[colInRow[j][i]]
		}
		if sum%2 == 1 {
			return false
		}
	}
	return true
}

func RunLDPC(prevHash []byte, curHash []byte) (int, []byte, [][]float64) {
	var LDPCNonce uint32
	var hashVector []int
	var outputWord []int
	var LRrtl [][]float64

	var currentBlockHeader string
	var currentBlockHeaderWithNonce string

	parameters := SetDifficultyUsingLevel(0)
	parameters.seed = GenerateSeed(prevHash)

	H := GenerateH(parameters)
	colInRow, rowInCol := GenerateQ(parameters, H)
	//PrintQ(printRowInCol)
	//PrintQ(printColInRow)

	for {
		//If Nonce is bigger than MaxNonce, then update timestamp
		if LDPCNonce >= MaxNonce {
			LDPCNonce = 0
			currentBlockHeader = string(curHash)
		}
		currentBlockHeaderWithNonce = currentBlockHeader + strconv.FormatUint(uint64(LDPCNonce), 10)

		hashVector = GenerateHv(parameters, []byte(currentBlockHeaderWithNonce))
		hashVector, outputWord, LRrtl = Decoding(parameters, hashVector, H, rowInCol, colInRow)
		flag := MakeDecision(parameters, colInRow, outputWord)

		if !flag {
			hashVector, outputWord, LRrtl = Decoding(parameters, hashVector, H, rowInCol, colInRow)
			flag = MakeDecision(parameters, colInRow, outputWord)
		}
		if flag {
			fmt.Printf("Codeword is founded with nonce = %d\n", LDPCNonce)
			break
		}
		LDPCNonce++
	}
	return int(LDPCNonce), crypto.Keccak256([]byte(currentBlockHeaderWithNonce)), LRrtl
}

//func isRegular(nSize, wCol, wRow int) bool {
//	res := float64(nSize*wCol) / float64(wRow)
//	m := math.Round(res)
//
//	if int(m)*wRow == nSize*wCol {
//		return true
//	}
//
//	return false
//}

//func SetDifficulty(nSize, wCol, wRow int) bool {
//	if isRegular(nSize, wCol, wRow) {
//		n = nSize
//		wc = wCol
//		wr = wRow
//		m = int(n * wc / wr)
//		return true
//	}
//	return false
//}

func funcF(x float64) float64 {
	if x >= BigInfinity {
		return 1.0 / BigInfinity
	} else if x <= (1.0 / BigInfinity) {
		return BigInfinity
	} else {
		return math.Log((math.Exp(x) + 1) / (math.Exp(x) - 1))
	}
}

func infinityTest(x float64) float64 {
	if x >= Inf {
		return Inf
	} else if x <= -Inf {
		return -Inf
	} else {
		return x
	}
}

//func newIntMatrix(rows, cols int) intMatrix {
//	m := intMatrix(make([][]int, rows))
//	for i := range m {
//		m[i] = make([]int, cols)
//	}
//	return m
//}
//
//func newFloatMatrix(rows, cols int) floatMatrix {
//	m := floatMatrix(make([][]float64, rows))
//	for i := range m {
//		m[i] = make([]float64, cols)
//	}
//	return m
//}

// NewShared creates a full sized ecc PoW shared between all requesters running
// in the same process.
//func NewShared() *ECC {
//	return &ecc{shared: sharedecc}
//}

func NewTester(notify []string, noverify bool) *ECC {
	ecc := &ECC{
		update:       make(chan struct{}),
		hashrate:     metrics.NewMeterForced(),
		workCh:       make(chan *sealTask),
		fetchWorkCh:  make(chan *sealWork),
		submitWorkCh: make(chan *mineResult),
		fetchRateCh:  make(chan chan uint64),
		submitRateCh: make(chan *hashrate),
		exitCh:       make(chan chan error),
	}
	go ecc.remote(notify, noverify)
	return ecc
}

// Close closes the exit channel to notify all backend threads exiting.
func (ecc *ECC) Close() error {
	var err error
	ecc.closeOnce.Do(func() {
		// Short circuit if the exit channel is not allocated.
		if ecc.exitCh == nil {
			return
		}
		errc := make(chan error)
		ecc.exitCh <- errc
		err = <-errc
		close(ecc.exitCh)
	})
	return err
}

// Threads returns the number of mining threads currently enabled. This doesn't
// necessarily mean that mining is running!
func (ecc *ECC) Threads() int {
	ecc.lock.Lock()
	defer ecc.lock.Unlock()

	return ecc.threads
}

// SetThreads updates the number of mining threads currently enabled. Calling
// this method does not start mining, only sets the thread count. If zero is
// specified, the miner will use all cores of the machine. Setting a thread
// count below zero is allowed and will cause the miner to idle, without any
// work being done.
func (ecc *ECC) SetThreads(threads int) {
	ecc.lock.Lock()
	defer ecc.lock.Unlock()

	// If we're running a shared PoW, set the thread count on that instead
	if ecc.shared != nil {
		ecc.shared.SetThreads(threads)
		return
	}
	// Update the threads and ping any running seal to pull in any changes
	ecc.threads = threads
	select {
	case ecc.update <- struct{}{}:
	default:
	}
}

// Hashrate implements PoW, returning the measured rate of the search invocations
// per second over the last minute.
// Note the returned hashrate includes local hashrate, but also includes the total
// hashrate of all remote miner.
func (ecc *ECC) Hashrate() float64 {
	// Short circuit if we are run the ecc in normal/test mode.

	var res = make(chan uint64, 1)

	select {
	case ecc.fetchRateCh <- res:
	case <-ecc.exitCh:
		// Return local hashrate only if ecc is stopped.
		return ecc.hashrate.Rate1()
	}

	// Gather total submitted hash rate of remote sealers.
	return ecc.hashrate.Rate1() + float64(<-res)
}

// APIs implements consensus.Engine, returning the user facing RPC APIs.
func (ecc *ECC) APIs(chain consensus.ChainReader) []rpc.API {
	// In order to ensure backward compatibility, we exposes ecc RPC APIs
	// to both eth and ecc namespaces.
	return []rpc.API{
		{
			Namespace: "eth",
			Version:   "1.0",
			Service:   &API{ecc},
			Public:    true,
		},
		{
			Namespace: "ecc",
			Version:   "1.0",
			Service:   &API{ecc},
			Public:    true,
		},
	}
}

//// SeedHash is the seed to use for generating a verification cache and the mining
//// dataset.
func SeedHash(block *types.Block) []byte {
	return block.ParentHash().Bytes()
}