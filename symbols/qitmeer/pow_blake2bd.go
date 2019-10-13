/**
Qitmeer
james
*/
package qitmeer

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"github.com/Qitmeer/go-opencl/cl"
	"github.com/Qitmeer/qitmeer/common/hash"
	"math/big"
	"qitmeer-miner/common"
	"qitmeer-miner/core"
	"qitmeer-miner/kernel"
	"sync"
	"time"
)

type Blake2bD struct {
	core.Device
	Work    *QitmeerWork
	header MinerBlockData
}

func (this *Blake2bD) InitDevice() {
	this.Started = time.Now().Unix()
	this.Device.InitDevice()
	if !this.IsValid {
		return
	}
	this.Program, this.Err = this.Context.CreateProgramWithSource([]string{kernel.DoubleBlake2bKernelSource})
	if this.Err != nil {
		common.MinerLoger.Errorf("#-%d,%s,%v CreateProgramWithSource", this.MinerId, this.DeviceName,this.Err )
		this.IsValid = false
		return
	}

	this.Err = this.Program.BuildProgram([]*cl.Device{this.ClDevice}, "")
	if this.Err != nil {
		common.MinerLoger.Errorf("-%d,%v BuildProgram", this.MinerId,this.Err )
		this.IsValid = false
		return
	}

	this.Kernel, this.Err = this.Program.CreateKernel("search")
	if this.Err != nil {
		common.MinerLoger.Errorf("-%d,%v CreateKernel", this.MinerId,this.Err )
		this.IsValid = false
		return
	}
	this.BlockObj, this.Err = this.Context.CreateEmptyBuffer(cl.MemReadOnly, 128)
	if this.Err != nil {
		common.MinerLoger.Errorf("-%d,%v CreateEmptyBuffer BlockObj", this.MinerId,this.Err )
		this.IsValid = false
		return
	}
	_ = this.Kernel.SetArgBuffer(0, this.BlockObj)
	this.NonceOutObj, this.Err = this.Context.CreateEmptyBuffer(cl.MemReadWrite, 8)
	if this.Err != nil {
		common.MinerLoger.Errorf("-%d,%v CreateEmptyBuffer NonceOutObj", this.MinerId,this.Err )
		this.IsValid = false
		return
	}
	this.NonceRandObj, this.Err = this.Context.CreateEmptyBuffer(cl.MemReadWrite, 8)
	if this.Err != nil {
		common.MinerLoger.Errorf("-%d,%v CreateEmptyBuffer NonceRandObj", this.MinerId,this.Err )
		this.IsValid = false
		return
	}
	this.Target2Obj, this.Err = this.Context.CreateEmptyBuffer(cl.MemReadWrite, 32)
	if this.Err != nil {
		common.MinerLoger.Errorf("-%d,%v CreateEmptyBuffer Target2Obj", this.MinerId,this.Err )
		this.IsValid = false
		return
	}
	_ = this.Kernel.SetArgBuffer(1, this.NonceOutObj)
	this.LocalItemSize, this.Err = this.Kernel.WorkGroupSize(this.ClDevice)
	this.LocalItemSize = this.Cfg.OptionConfig.WorkSize
	if this.Err != nil {
		common.MinerLoger.Infof("- WorkGroupSize failed -%d %v", this.MinerId,this.Err )
		this.IsValid = false
		return
	}
	_ = this.Kernel.SetArgBuffer(2, this.NonceRandObj)
	_ = this.Kernel.SetArgBuffer(3, this.Target2Obj)
	common.MinerLoger.Debugf("- Device ID:%d- Global item size:%d- Local item size:%d",this.MinerId, this.GlobalItemSize, this.LocalItemSize)
	this.NonceOut = make([]byte, 8)
	if this.Event, this.Err = this.CommandQueue.EnqueueWriteBufferByte(this.NonceOutObj, true, 0, this.NonceOut, nil); this.Err != nil {
		common.MinerLoger.Errorf("-%d %v EnqueueWriteBufferByte NonceOutObj", this.MinerId,this.Err )
		this.IsValid = false
		return
	}
	this.Event.Release()
}

func (this *Blake2bD) Update() {
	//update coinbase tx hash
	this.Device.Update()
	if this.Pool {
		this.Work.PoolWork.ExtraNonce2 = fmt.Sprintf("%08x", uint32(this.CurrentWorkID))
		this.header.Exnonce2 = this.Work.PoolWork.ExtraNonce2
		this.Work.PoolWork.WorkData = this.Work.PoolWork.PrepQitmeerWork()
		this.header.PackagePoolHeader(this.Work)
	} else {
		randStr := fmt.Sprintf("%s%d",this.Cfg.SoloConfig.RandStr,this.CurrentWorkID)
		txHash := this.Work.Block.CalcCoinBase(this.Cfg,randStr,this.CurrentWorkID,this.Cfg.SoloConfig.MinerAddr)
		this.header.PackageRpcHeader(this.Work)
		this.header.HeaderBlock.TxRoot = *txHash
	}
}

func (this *Blake2bD) Mine(wg *sync.WaitGroup) {
	defer wg.Done()
	defer this.Release()
	var randNonceBase ,xnonce uint64
	var subm string
	var txCount,j int
	var h hash.Hash
	var w core.BaseWork
	randNonceBytes := make([]byte,8)
	offset := 0
	for {

		select {
		case w = <-this.NewWork:
			this.Work = w.(*QitmeerWork)
		case <-this.Quit:
			return
		default:

		}
		if !this.IsValid {
			common.MinerLoger.Errorf("# %d %s device not use to mining.",this.MinerId,this.DeviceName)
			time.Sleep(2*time.Second)
			continue
		}
		if !this.HasNewWork || this.Work == nil{
			continue
		}
		if len(this.Work.PoolWork.WorkData) <= 0 && this.Work.Block.Height <= 0 {
			continue
		}
		this.Started = time.Now().Unix()
		this.AllDiffOneShares = 0
		this.HasNewWork = false
		this.CurrentWorkID = 0
		this.header = MinerBlockData{
			Transactions:[]Transactions{},
			Parents:[]ParentItems{},
			HeaderData:make([]byte,0),
			TargetDiff:&big.Int{},
			JobID:"",
			Exnonce2:"",
		}

		for {
			// if has new work ,current calc stop
			if this.HasNewWork {
				break
			}
			if !this.IsValid {
				break
			}

			this.Update()
			if this.Event, this.Err = this.CommandQueue.EnqueueWriteBufferByte(this.BlockObj, true, 0, BlockData(this.header.HeaderBlock), nil); this.Err != nil {
				common.MinerLoger.Errorf("-%d %v %v %d EnqueueWriteBufferByte BlockObj", this.MinerId,this.Err ,this.BlockObj,len(BlockData(this.header.HeaderBlock)))
				this.IsValid = false
				return
			}
			this.Event.Release()
			randNonceBase,_ = common.RandUint64()
			randNonceBytes = make([]byte,8)
			binary.LittleEndian.PutUint64(randNonceBytes,randNonceBase)
			if this.Event, this.Err = this.CommandQueue.EnqueueWriteBufferByte(this.NonceRandObj, true, 0, randNonceBytes, nil); this.Err != nil {
				common.MinerLoger.Errorf("-%d %v EnqueueWriteBufferByte NonceRandObj", this.MinerId,this.Err )
				this.IsValid = false
				return
			}
			this.Event.Release()
			if this.Event, this.Err = this.CommandQueue.EnqueueWriteBufferByte(this.Target2Obj, true, 0, this.header.Target2, nil); this.Err != nil {
				common.MinerLoger.Errorf("-%d %v EnqueueWriteBufferByte Target2Obj", this.MinerId,this.Err )
				this.IsValid = false
				return
			}
			this.Event.Release()
			//Run the kernel
			if this.Event, this.Err = this.CommandQueue.EnqueueNDRangeKernel(this.Kernel, []int{int(offset)}, []int{this.GlobalItemSize}, []int{this.LocalItemSize}, nil); this.Err != nil {
				common.MinerLoger.Errorf("-%d %v EnqueueNDRangeKernel Kernel", this.MinerId,this.Err )
				this.IsValid = false
				return
			}
			this.Event.Release()
			//Get output
			if this.Event, this.Err = this.CommandQueue.EnqueueReadBufferByte(this.NonceOutObj, true, 0, this.NonceOut, nil); this.Err != nil {
				common.MinerLoger.Errorf("-%d %v EnqueueReadBufferByte NonceOutObj", this.MinerId,this.Err )
				this.IsValid = false
				return
			}
			this.Event.Release()
			this.AllDiffOneShares += uint64(this.GlobalItemSize)
			xnonce = binary.LittleEndian.Uint64(this.NonceOut)
			if xnonce >0 {
				//Found Hash
				this.header.HeaderBlock.Nonce = xnonce
				h = this.header.HeaderBlock.BlockHash()
				common.MinerLoger.Debugf("device #%d found hash:%s nonce:%d target:%064x",this.MinerId,h,xnonce,this.header.TargetDiff)
				if HashToBig(&h).Cmp(this.header.TargetDiff) <= 0 {
					subm = hex.EncodeToString(BlockData(this.header.HeaderBlock))
					if !this.Pool{
						subm += common.Int2varinthex(int64(len(this.header.Parents)))
						for j = 0; j < len(this.header.Parents); j++ {
							subm += this.header.Parents[j].Data
						}

						txCount = len(this.header.Transactions) //real transaction count except coinbase
						subm += common.Int2varinthex(int64(txCount))

						for j = 0; j < txCount; j++ {
							subm += this.header.Transactions[j].Data
						}
						txCount -= 1
						subm += "-" + fmt.Sprintf("%d",txCount) + "-" + fmt.Sprintf("%d",this.header.HeaderBlock.ExNonce)
					} else {
						subm += "-" + this.header.JobID + "-" + this.header.Exnonce2
					}
					this.SubmitData <- subm
					if !this.Pool{
						//solo wait new task
						this.ClearNonceData()
						break
					}
				}
			}
			this.ClearNonceData()
		}
	}
}

func (this* Blake2bD) ClearNonceData()  {
	this.NonceOut = make([]byte, 8)
	if this.Event, this.Err = this.CommandQueue.EnqueueWriteBufferByte(this.NonceOutObj, true, 0, this.NonceOut, nil); this.Err != nil {
		common.MinerLoger.Errorf("-%d %v EnqueueWriteBufferByte", this.MinerId,this.Err )
		this.IsValid = false
		return
	}
	this.Event.Release()
}