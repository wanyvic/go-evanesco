package zkpminer

import (
	"errors"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/zkpminer/keypair"
	"github.com/ethereum/go-ethereum/zkpminer/problem"
	"github.com/ethereum/go-ethereum/zkpminer/vrf"
	"sync"
	"sync/atomic"
)

var (
	InvalidStepError = errors.New("invalid task step")
	ZKPProofError    = errors.New("zkp prove error")
)

type Worker struct {
	mu               sync.RWMutex
	running          int32
	MaxTaskCnt       int32
	CoinbaseAddr     common.Address
	minerAddr        common.Address
	pk               *keypair.PublicKey
	sk               *keypair.PrivateKey
	workingTaskCnt   int32
	coinbaseInterval Height
	submitAdvance    Height
	inboundTaskCh    chan *Task //channel to get task from miner
	scanner          *Scanner
	zkpProver        *problem.Prover
	exitCh           chan struct{}
}

func (w *Worker) Loop() {
	for {
		select {
		case <-w.exitCh:
			atomic.StoreInt32(&w.running, 0)
			return
		case task := <-w.inboundTaskCh:
			if !w.isRunning() {
				continue
			}
			if task.Step == TASKSTART {
				go func() {
					atomic.AddInt32(&w.workingTaskCnt, int32(1))
					defer atomic.AddInt32(&w.workingTaskCnt, int32(-1))
					err := w.HandleStartTask(task)
					if err != nil {
						log.Error(err.Error())
					}
				}()
				continue
			}
			if task.Step == TASKGETCHALLENGEBLOCK {
				go func() {
					atomic.AddInt32(&w.workingTaskCnt, int32(1))
					defer atomic.AddInt32(&w.workingTaskCnt, int32(-1))
					err := w.HandleChallengedTask(task)
					if err != nil {
						log.Error(err.Error())
					}
				}()
				continue
			}
		}
	}
}

func (w *Worker) isRunning() bool {
	return atomic.LoadInt32(&w.running) == 1
}

func (w *Worker) start() {
	atomic.StoreInt32(&w.running, 1)
}

func (w *Worker) stop() {
	atomic.StoreInt32(&w.running, 0)
}

func (w *Worker) close() {
	defer func() {
		if recover() != nil {
		}
	}()
	atomic.StoreInt32(&w.running, 0)
	close(w.exitCh)
}

func (w *Worker) HandleStartTask(task *Task) error {
	log.Debug("handle start task")
	task.minerAddr = w.minerAddr
	task.lottery.SetMinerAddr(w.minerAddr)
	index, proof := vrf.Evaluate(w.sk, task.lastCoinBaseHash[:])
	task.challengeIndex = Height(problem.GetChallengeIndex(index, uint64(w.coinbaseInterval)-uint64(w.submitAdvance)))

	task.lottery.VrfProof = proof
	task.lottery.Index = index
	task.Step = TASKWAITCHALLENGEBLOCK

	log.Info("vrf finished", "challenge height:", w.scanner.LastCoinbaseHeight+task.challengeIndex, "index:", task.challengeIndex)
	log.Info("waiting for challenge block", "time duration (second)", task.challengeIndex*6)
	// request if this block already exit
	header, err := w.scanner.GetHeader(w.scanner.LastCoinbaseHeight + task.challengeIndex)
	if header != nil && err == nil {
		return w.HandleTaskAfterChallenge(header, task)
	}

	// waiting for challenge block exist
	return w.HandlerTaskBeforeChallenge(task)
}

func (w *Worker) HandleChallengedTask(task *Task) error {
	log.Info("start working ZKP problem")
	// start zkp proof
	err := w.SolveProblem(task)
	if err != nil {
		log.Error("solve zkp problem error", "err", err)
		return err
	}
	log.Info("ZKP problem finished")
	//give it to miner to submit
	w.scanner.inboundTaskCh <- task
	return nil
}

func (w *Worker) HandleTaskAfterChallenge(header *types.Header, task *Task) error {
	log.Debug("handler task after challenge")
	task.SetHeader(header)
	return w.HandleChallengedTask(task)
}

func (w *Worker) HandlerTaskBeforeChallenge(task *Task) error {
	log.Debug("handle task before challenge", "index", task.challengeIndex)
	task.Step = TASKWAITCHALLENGEBLOCK
	w.scanner.inboundTaskCh <- task
	return nil
}

func (w *Worker) SolveProblem(task *Task) error {
	if task.Step != TASKGETCHALLENGEBLOCK {
		return InvalidStepError
	}
	// preimage: keccak(address || challenge hash)
	addrBytes := task.minerAddr.Bytes()
	preimage := append(addrBytes, task.lottery.ChallengeHeaderHash[:]...)
	preimage = crypto.Keccak256(preimage)
	mimcHash, proof := w.zkpProver.Prove(preimage)
	if mimcHash == nil || proof == nil {
		return ZKPProofError
	}
	task.lottery.ZkpProof = proof
	task.lottery.MimcHash = mimcHash

	err := w.SignLottery(task)
	if err != nil {
		return err
	}
	task.Step = TASKPROBLEMSOLVED
	return nil
}

func (w *Worker) SignLottery(task *Task) error {
	b, err := task.lottery.Serialize()
	if err != nil {
		return err
	}
	hash := crypto.Keccak256(b)
	sig, err := crypto.Sign(hash, w.sk.PrivateKey)
	if err != nil {
		return err
	}
	copy(task.signature[:], sig)
	return nil
}
