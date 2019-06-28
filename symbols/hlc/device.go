/**
HLC FOUNDATION
james
*/
package hlc

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"github.com/HalalChain/qitmeer-lib/common/hash"
	"github.com/robvanmieghem/go-opencl/cl"
	"hlc-miner/common"
	"hlc-miner/core"
	"hlc-miner/cuckoo"
	"log"
	"math/big"
	"sort"
	"sync/atomic"
	"unsafe"
)

type HLCDevice struct {
	core.Device
	ClearBytes	[]byte
	EdgesObj              *cl.MemObject
	EdgesBytes            []byte
	DestinationEdgesCountObj              *cl.MemObject
	DestinationEdgesCountBytes            []byte
	EdgesIndexObj         *cl.MemObject
	EdgesIndexBytes       []byte
	DestinationEdgesObj   *cl.MemObject
	DestinationEdgesBytes []byte
	NoncesObj             *cl.MemObject
	NoncesBytes           []byte
	Nonces           []uint32
	NodesObj              *cl.MemObject
	NodesBytes            []byte
	Edges                 []uint32
	CreateEdgeKernel      *cl.Kernel
	Trimmer01Kernel       *cl.Kernel
	Trimmer02Kernel       *cl.Kernel
	RecoveryKernel        *cl.Kernel
	NewWork               chan HLCWork
	Work                  HLCWork
	Transactions                  map[int][]Transactions
	header MinerBlockData
}

func (this *HLCDevice) InitDevice() {
	this.Device.InitDevice()
	if !this.IsValid {
		return
	}
	var err error
	this.Program, err = this.Context.CreateProgramWithSource([]string{cuckoo.NewKernel})
	if err != nil {
		log.Println("-", this.MinerId, this.DeviceName, err)
		this.IsValid = false
		return
	}

	err = this.Program.BuildProgram([]*cl.Device{this.ClDevice}, "")
	if err != nil {
		log.Println("-", this.MinerId, err)
		this.IsValid = false
		return
	}

	this.InitKernelAndParam()

}

func (this *HLCDevice) Update() {
	//update coinbase tx hash
	this.Device.Update()
	if this.Pool {
		//this.CurrentWorkID = 0
		//randstr := fmt.Sprintf("%dhlcminer%d",this.CurrentWorkID,this.MinerId)
		//byt := []byte(randstr)[:4]
		//this.Work.PoolWork.ExtraNonce2 = hex.EncodeToString(byt)
		this.Work.PoolWork.ExtraNonce2 = fmt.Sprintf("%08x", this.CurrentWorkID)
		this.Work.PoolWork.WorkData = this.Work.PoolWork.PrepHlcWork()
	} else {
		randStr := fmt.Sprintf("%s%d%d", this.Cfg.RandStr, this.MinerId, this.CurrentWorkID)
		var err error
		err = this.Work.Block.CalcCoinBase(randStr, this.Cfg.MinerAddr)
		if err != nil {
			log.Println("calc coinbase error :", err)
			return
		}
		this.Work.Block.BuildMerkleTreeStore()
	}
}

func (this *HLCDevice) Mine() {

	defer this.Release()

	for {
		select {
		case this.Work = <-this.NewWork:
		case <-this.Quit:
			return

		}
		if !this.IsValid {
			continue
		}

		if len(this.Work.PoolWork.WorkData) <= 0 && this.Work.Block.Height <= 0 {
			continue
		}

		this.HasNewWork = false
		this.CurrentWorkID = 0
		var err error
		for {
			// if has new work ,current calc stop
			if this.HasNewWork {
				break
			}
			this.header = MinerBlockData{
				Transactions:[]Transactions{},
				Parents:[]ParentItems{},
				HeaderData:make([]byte,0),
				TargetDiff:&big.Int{},
				JobID:"",
			}
			this.Update()
			if this.Pool {
				this.header.PackagePoolHeader(&this.Work)
			} else {
				this.header.PackageRpcHeader(&this.Work)
			}
			this.Transactions[int(this.MinerId)] = make([]Transactions,0)
			for k := 0;k<len(this.header.Transactions);k++{
				this.Transactions[int(this.MinerId)] = append(this.Transactions[int(this.MinerId)],Transactions{
					Data:this.header.Transactions[k].Data,
					Hash:this.header.Transactions[k].Hash,
					Fee:this.header.Transactions[k].Fee,
				})
			}

			hdrkey := hash.DoubleHashH(this.header.HeaderData[0:NONCEEND])
			sip := cuckoo.Newsip(hdrkey[:16])

			this.InitParamData()
			err = this.CreateEdgeKernel.SetArg(0,uint64(sip.V[0]))
			if err != nil {
				log.Println("-", this.MinerId, err)
				this.IsValid = false
				return
			}
			err = this.CreateEdgeKernel.SetArg(1,uint64(sip.V[1]))
			if err != nil {
				log.Println("-", this.MinerId, err)
				this.IsValid = false
				return
			}
			err = this.CreateEdgeKernel.SetArg(2,uint64(sip.V[2]))
			if err != nil {
				log.Println("-", this.MinerId, err)
				this.IsValid = false
				return
			}
			err = this.CreateEdgeKernel.SetArg(3,uint64(sip.V[3]))
			if err != nil {
				log.Println("-", this.MinerId, err)
				this.IsValid = false
				return
			}

			// 2 ^ 24 2 ^ 11 * 2 ^ 8 * 2 * 2 ^ 4 11+8+1+4=24
			if _, err = this.CommandQueue.EnqueueNDRangeKernel(this.CreateEdgeKernel, []int{0}, []int{2048*256*2}, []int{256}, nil); err != nil {
				log.Println("CreateEdgeKernel-1058", this.MinerId,err)
				return
			}
			for i:= 0;i<this.Cfg.TrimmerCount;i++{
				if _, err = this.CommandQueue.EnqueueNDRangeKernel(this.Trimmer01Kernel, []int{0}, []int{2048*256*2}, []int{256}, nil); err != nil {
					log.Println("Trimmer01Kernel-1058", this.MinerId,err)
					return
				}
			}
			if _, err = this.CommandQueue.EnqueueNDRangeKernel(this.Trimmer02Kernel, []int{0}, []int{2048*256*2}, []int{256}, nil); err != nil {
				log.Println("Trimmer02Kernel-1058", this.MinerId,err)
				return
			}
			this.DestinationEdgesCountBytes = make([]byte,8)
			_,err = this.CommandQueue.EnqueueReadBufferByte(this.DestinationEdgesCountObj,true,0,this.DestinationEdgesCountBytes,nil)
			count := binary.LittleEndian.Uint32(this.DestinationEdgesCountBytes[4:8])
			if count >= cuckoo.PROOF_SIZE*2{
				this.DestinationEdgesBytes = make([]byte,count*2*4)
				_,err = this.CommandQueue.EnqueueReadBufferByte(this.DestinationEdgesObj,true,0,this.DestinationEdgesBytes,nil)
				this.Edges = make([]uint32,0)
				for j:=0;j<len(this.DestinationEdgesBytes);j+=4{
					this.Edges = append(this.Edges,binary.LittleEndian.Uint32(this.DestinationEdgesBytes[j:j+4]))
				}
				cg := cuckoo.CGraph{}
				cg.SetEdges(this.Edges,int(count))
				atomic.AddUint64(&this.AllDiffOneShares, 1)
				if cg.FindSolutions(){
					//if cg.FindCycle(){
					_,err = this.CommandQueue.EnqueueWriteBufferByte(this.NodesObj,true,0,cg.GetNonceEdgesBytes(),nil)
					if _, err = this.CommandQueue.EnqueueNDRangeKernel(this.RecoveryKernel, []int{0}, []int{2048*256*2}, []int{256}, nil); err != nil {
						log.Println("RecoveryKernel-1058", this.MinerId,err)
						return
					}
					this.NoncesBytes = make([]byte,4*cuckoo.PROOF_SIZE)
					_,err = this.CommandQueue.EnqueueReadBufferByte(this.NoncesObj,true,0,this.NoncesBytes,nil)
					this.Nonces = make([]uint32,0)
					for j := 0;j<cuckoo.PROOF_SIZE*4;j+=4{
						this.Nonces = append(this.Nonces,binary.LittleEndian.Uint32(this.NoncesBytes[j:j+4]))
					}

					sort.Slice(this.Nonces, func(i, j int) bool {
						return this.Nonces[i] < this.Nonces[j]
					})
					if err = cuckoo.Verify(hdrkey[:16],this.Nonces);err == nil{
						for i := 0; i < len(this.Nonces); i++ {
							b := make([]byte,4)
							binary.LittleEndian.PutUint32(b,this.Nonces[i])
							this.header.HeaderData = append(this.header.HeaderData,b...)
						}
						h := hash.DoubleHashH(this.header.HeaderData)
						log.Println("[Calc Hash]",h)
						log.Println(fmt.Sprintf("[Target Hash] %064x",this.header.TargetDiff))
						if HashToBig(&h).Cmp(this.header.TargetDiff) <= 0 {
							subm := hex.EncodeToString(this.header.HeaderData)
							if !this.Pool{
								if this.Cfg.DAG{
									subm += common.Int2varinthex(int64(len(this.header.Parents)))
									for j := 0; j < len(this.header.Parents); j++ {
										subm += this.header.Parents[j].Data
									}
								}

								txCount := len(this.Transactions)
								subm += common.Int2varinthex(int64(txCount))

								for j := 0; j < txCount; j++ {
									subm += this.Transactions[int(this.MinerId)][j].Data
								}
								txCount -= 1 //real transaction count except coinbase
								subm += "-" + fmt.Sprintf("%d",txCount) + "-" + fmt.Sprintf("%d",this.Work.Block.Height)
							} else {
								subm += "-" + this.header.JobID + "-" + this.Work.PoolWork.ExtraNonce2
							}
							this.SubmitData <- subm
							if !this.Pool{
								//solo wait new task
								break
							}
						}

					} else{
						log.Println("result not match:",err)
					}
				}
			}
		}
	}
}

func (this *HLCDevice) SubmitShare(substr chan string) {
	for {
		select {
		case <-this.Quit:
			return
		case str := <-this.SubmitData:
			if this.HasNewWork {
				//the stale submit
				continue
			}
			substr <- str
		}
	}
}

func (this *HLCDevice) Release() {
	this.Context.Release()
	this.Program.Release()
	this.CreateEdgeKernel.Release()
	this.Trimmer01Kernel.Release()
	this.Trimmer02Kernel.Release()
	this.RecoveryKernel.Release()
	this.EdgesObj.Release()
	this.EdgesIndexObj.Release()
	this.DestinationEdgesObj.Release()
	this.NoncesObj.Release()
	this.NodesObj.Release()
}

func (this *HLCDevice) InitParamData() {
	var err error
	this.ClearBytes = make([]byte,4)
	_,err = this.CommandQueue.EnqueueFillBuffer(this.EdgesIndexObj,unsafe.Pointer(&this.ClearBytes[0]),4,0,cuckoo.EDGE_SIZE*8,nil)
	if err != nil {
		log.Println("-", this.MinerId, err)
		this.IsValid = false
		return
	}
	_,err = this.CommandQueue.EnqueueFillBuffer(this.EdgesObj,unsafe.Pointer(&this.ClearBytes[0]),4,0,cuckoo.EDGE_SIZE*8,nil)
	if err != nil {
		log.Println("-", this.MinerId, err)
		this.IsValid = false
		return
	}
	_,err = this.CommandQueue.EnqueueFillBuffer(this.DestinationEdgesObj,unsafe.Pointer(&this.ClearBytes[0]),4,0,cuckoo.EDGE_SIZE*8,nil)
	if err != nil {
		log.Println("-", this.MinerId, err)
		this.IsValid = false
		return
	}
	_,err = this.CommandQueue.EnqueueFillBuffer(this.NodesObj,unsafe.Pointer(&this.ClearBytes[0]),4,0,cuckoo.PROOF_SIZE*8,nil)
	if err != nil {
		log.Println("-", this.MinerId, err)
		this.IsValid = false
		return
	}
	_,err = this.CommandQueue.EnqueueFillBuffer(this.DestinationEdgesCountObj,unsafe.Pointer(&this.ClearBytes[0]),4,0,8,nil)
	if err != nil {
		log.Println("-", this.MinerId, err)
		this.IsValid = false
		return
	}
	_,err = this.CommandQueue.EnqueueFillBuffer(this.NoncesObj,unsafe.Pointer(&this.ClearBytes[0]),4,0,cuckoo.PROOF_SIZE*4,nil)
	if err != nil {
		log.Println("-", this.MinerId, err)
		this.IsValid = false
		return
	}

	err = this.CreateEdgeKernel.SetArgBuffer(4,this.EdgesObj)
	err = this.CreateEdgeKernel.SetArgBuffer(5,this.EdgesIndexObj)

	err = this.Trimmer01Kernel.SetArgBuffer(0,this.EdgesObj)
	err = this.Trimmer01Kernel.SetArgBuffer(1,this.EdgesIndexObj)

	err = this.Trimmer02Kernel.SetArgBuffer(0,this.EdgesObj)
	err = this.Trimmer02Kernel.SetArgBuffer(1,this.EdgesIndexObj)
	err = this.Trimmer02Kernel.SetArgBuffer(2,this.DestinationEdgesObj)
	err = this.Trimmer02Kernel.SetArgBuffer(3,this.DestinationEdgesCountObj)

	err = this.RecoveryKernel.SetArgBuffer(0,this.EdgesObj)
	err = this.RecoveryKernel.SetArgBuffer(1,this.NodesObj)
	err = this.RecoveryKernel.SetArgBuffer(2,this.NoncesObj)
}

func (this *HLCDevice) InitKernelAndParam() {
	var err error
	this.CreateEdgeKernel, err = this.Program.CreateKernel("CreateEdges")
	if err != nil {
		log.Println("-", this.MinerId, err)
		this.IsValid = false
		return
	}

	this.Trimmer01Kernel, err = this.Program.CreateKernel("Trimmer01")
	if err != nil {
		log.Println("-", this.MinerId, err)
		this.IsValid = false
		return
	}

	this.Trimmer02Kernel, err = this.Program.CreateKernel("Trimmer02")
	if err != nil {
		log.Println("-", this.MinerId, err)
		this.IsValid = false
		return
	}

	this.RecoveryKernel, err = this.Program.CreateKernel("RecoveryNonce")
	if err != nil {
		log.Println("-", this.MinerId, err)
		this.IsValid = false
		return
	}

	this.EdgesObj, err = this.Context.CreateEmptyBuffer(cl.MemReadWrite, cuckoo.EDGE_SIZE*2*4)
	if err != nil {
		log.Println("-", this.MinerId, err)
		this.IsValid = false
		return
	}
	this.DestinationEdgesObj, err = this.Context.CreateEmptyBuffer(cl.MemReadWrite, cuckoo.EDGE_SIZE*2*4)
	if err != nil {
		log.Println("-", this.MinerId, err)
		this.IsValid = false
		return
	}
	this.NodesObj, err = this.Context.CreateEmptyBuffer(cl.MemReadWrite, cuckoo.PROOF_SIZE*4*2)
	if err != nil {
		log.Println("-", this.MinerId, err)
		this.IsValid = false
		return
	}
	this.EdgesIndexObj, err = this.Context.CreateEmptyBuffer(cl.MemReadWrite, cuckoo.EDGE_SIZE*4*2)
	if err != nil {
		log.Println("-", this.MinerId, err)
		this.IsValid = false
		return
	}
	this.DestinationEdgesCountObj, err = this.Context.CreateEmptyBuffer(cl.MemReadWrite, 8)
	if err != nil {
		log.Println("-", this.MinerId, err)
		this.IsValid = false
		return
	}
	this.NoncesObj, err = this.Context.CreateEmptyBuffer(cl.MemReadWrite, cuckoo.PROOF_SIZE*4)
	if err != nil {
		log.Println("-", this.MinerId, err)
		this.IsValid = false
		return
	}

}
