package bgp

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/CSUNetSec/bgpmon/log"
	"github.com/CSUNetSec/bgpmon/module"
	"github.com/CSUNetSec/bgpmon/session"

	pbbgpmon "github.com/CSUNetSec/netsec-protobufs/bgpmon"
)

const (
	asNumberByPrefixStmt    = "SELECT timestamp, dateOf(timestamp), prefix_ip_address, prefix_mask, as_number, is_advertisement FROM %s.as_number_by_prefix_range WHERE time_bucket=? AND prefix_ip_address>=? AND prefix_ip_address<=?"
	updateMessageSelectStmt = "SELECT as_path, peer_ip_address, collector_ip_address FROM csu_bgp_core.update_messages_by_time WHERE time_bucket=? AND timestamp=?"
	prefixHijacksStmt       = "INSERT INTO csu_bgp_derived.prefix_hijacks(time_bucket, timestamp, module_id, advertised_ip_address, advertised_mask, monitor_ip_address, monitor_mask) VALUES(?,?,?,?,?,?,?)"
)

//struct for use in parsing bgpmond toml configuration file
type PrefixHijackConfig struct {
	Keyspaces []string
}

type PrefixHijackModule struct {
	prefixCache     *PrefixCache
	periodicSeconds int32
	timeoutSeconds  int32
	inSessions      []session.CassandraSession
	keyspaces       []string
	status          *PrefixHijackStatus
	hijackUUIDs     []string
}

type PrefixHijackStatus struct {
	ExecutionCount    uint
	LastExecutionTime time.Time
}

func NewPrefixHijackModule(monitorPrefixes []*pbbgpmon.PrefixHijackModule_MonitorPrefix, periodicSeconds, timeoutSeconds int32, inSessions []session.Sessioner, config PrefixHijackConfig) (*module.Module, error) {
	//check that all sessions are cassandra sessions
	inSess := []session.CassandraSession{}
	for _, sess := range inSessions {
		casSess, ok := sess.(session.CassandraSession)
		if !ok {
			return nil, errors.New("Only cassandra sessions are supported for prefix hijack module")
		}

		inSess = append(inSess, casSess)
	}

	//populate prefix cache
	prefixCache := NewPrefixCache()
	for _, monitorPrefix := range monitorPrefixes {
		prefixCache.AddPrefix(monitorPrefix.Prefix.Prefix.Ipv4, monitorPrefix.Prefix.Mask, monitorPrefix.AsNumber)
	}

	return &module.Module{Moduler: PrefixHijackModule{prefixCache, periodicSeconds, timeoutSeconds, inSess, config.Keyspaces, &PrefixHijackStatus{0, time.Now()}, []string{}}}, nil
}

func (p PrefixHijackModule) Run() error {
	log.Debl.Printf("Running prefix hijack module\n")

	//get execution time and initialize timebuckets to today and yesterday
	executionTime := time.Now().UTC()
	timeBuckets := []time.Time{getTimeBucket(executionTime), getTimeBucket(time.Unix(executionTime.Unix()-86400, 0))}

	//loop through time buckets
	var (
		timeuuid        string
		timestamp       time.Time
		ipAddress       string
		mask, asNumber  uint32
		isAdvertisement bool

		asPath                            []int
		peerIpAddress, collectorIpAddress string
	)

	for _, timeBucket := range timeBuckets {
		for _, session := range p.inSessions {
			for _, keyspace := range p.keyspaces {
				for _, prefixNode := range p.prefixCache.prefixNodes {
					//fmt.Printf("CHECKING FOR HIJACKS ON %s/%d\n", prefixNode.ipAddress, prefixNode.mask)
					prefixRangeIter := session.CqlSession.Query(fmt.Sprintf(asNumberByPrefixStmt, keyspace), timeBucket, prefixNode.minAddress, prefixNode.maxAddress).Iter()
					for prefixRangeIter.Scan(&timeuuid, &timestamp, &ipAddress, &mask, &asNumber, &isAdvertisement) {
						//make sure the message is an advertisement and not withdrawl
						if !isAdvertisement {
							continue
						}

						//check for valid mask, timestamp, and if source is a valid asNumber
						if mask < prefixNode.mask {
							continue
						}

						//check if potential hijack has already been seen
						for _, hijackUUID := range p.hijackUUIDs {
							if hijackUUID == timeuuid {
								continue
							}
						}

						//retrieve as path of message - query update_messages_by_time with timeuuid
						updateMessageIter := session.CqlSession.Query(updateMessageSelectStmt, timeBucket, timeuuid).Iter()
						if updateMessageIter.Scan(&asPath, &peerIpAddress, &collectorIpAddress) {
							found := false
							for _, asNum := range asPath {
								if !prefixNode.ValidAsNumber(uint32(asNum)) {
									found = true
									break
								}
							}

							if found {
								continue
							}
						} else {
							//if message not found only check the source as on as_numbers_by_prefix_range
							if !prefixNode.ValidAsNumber(uint32(asNumber)) {
								continue
							}
						}

						//TODO check historical data by querying prefix_by_as_number

						fmt.Printf("\tNOTIFICATION OF HIJACK - TIMESTAMP:%v IP_ADDRESS:%s MASK:%d AS_PATH:%d\n", timestamp, ipAddress, mask, asNumber)
						p.hijackUUIDs = append(p.hijackUUIDs, timeuuid)

						//write hijack to cassandra - TODO get module id from somewhere
						err := session.CqlSession.Query(prefixHijacksStmt, timeBucket, timeuuid, "", ipAddress, mask, prefixNode.ipAddress, prefixNode.mask).Exec()
						if err != nil {
							return err
						}
					}
				}
			}
		}
	}

	//update status variables
	p.status.ExecutionCount++
	p.status.LastExecutionTime = executionTime
	return nil
}

func (p PrefixHijackModule) Status() string {
	return fmt.Sprintf("%v", p.status)
}

func (p PrefixHijackModule) Cleanup() error {
	return nil
}

/*
 * PrefixCache
 */
type PrefixCache struct {
	roots       []*PrefixNode
	prefixNodes []*PrefixNode
}

func NewPrefixCache() *PrefixCache {
	return &PrefixCache{
		roots:       []*PrefixNode{},
		prefixNodes: []*PrefixNode{},
	}
}

func (p *PrefixCache) AddPrefix(ipAddress net.IP, mask uint32, asNumbers []uint32) error {
	//create PrefxNode
	prefixNode := NewPrefixNode(&ipAddress, mask, asNumbers)
	p.prefixNodes = append(p.prefixNodes, prefixNode)

	//check if prefixNode is subprefix/superprefix of a root
	removeIndex := -1
	for i, node := range p.roots {
		if prefixNode.SubPrefix(node) {
			//find correct node to insert on
			insertNode := node
			found := true
			for found {
				found = false
				for _, child := range insertNode.children {
					if prefixNode.SubPrefix(child) {
						insertNode = child
						found = true
					}
				}
			}

			//check if it's a superprefix to any children
			superPrefixIndex := -1
			for i, child := range insertNode.children {
				if prefixNode.SuperPrefix(child) {
					superPrefixIndex = i
					break
				}
			}

			if superPrefixIndex != -1 {
				prefixNode.parent = insertNode
				insertNode.children[superPrefixIndex].parent = prefixNode

				prefixNode.children = append(prefixNode.children, insertNode.children[superPrefixIndex])
				insertNode.children = append(insertNode.children[:superPrefixIndex], insertNode.children[superPrefixIndex+1:]...)
			} else {
				prefixNode.parent = insertNode
				insertNode.children = append(insertNode.children, prefixNode)
			}

			return nil
		} else if prefixNode.SuperPrefix(node) {
			//add prefixNode as superprefix to node
			node.parent = prefixNode
			prefixNode.children = append(prefixNode.children, node)

			removeIndex = i
			break
		}
	}

	if removeIndex != -1 {
		//remove value
		p.roots = append(p.roots[:removeIndex], p.roots[removeIndex+1:]...)
	}

	p.roots = append(p.roots, prefixNode)
	return nil
}

func (p *PrefixCache) Print() {
	for _, root := range p.roots {
		root.Print(0)
	}
}

type PrefixNode struct {
	ipAddress              *net.IP
	mask                   uint32
	asNumbers              []uint32
	minAddress, maxAddress []byte
	parent                 *PrefixNode
	children               []*PrefixNode
}

func NewPrefixNode(ipAddress *net.IP, mask uint32, asNumbers []uint32) *PrefixNode {
	minAddress, maxAddress, _ := getIPRange(*ipAddress, int(mask))

	return &PrefixNode{
		ipAddress:  ipAddress,
		mask:       mask,
		asNumbers:  asNumbers,
		minAddress: minAddress,
		maxAddress: maxAddress,
		parent:     nil,
		children:   []*PrefixNode{},
	}
}

func (p *PrefixNode) SubPrefix(prefixNode *PrefixNode) bool {
	//check if p.mask is shorter
	if p.mask <= prefixNode.mask {
		return false
	}

	//check if p.minAddress < prefixNode.minAddress or p.maxAddress > prefixNode.maxAddress
	if bytes.Compare(p.minAddress, prefixNode.minAddress) < 0 || bytes.Compare(p.maxAddress, prefixNode.maxAddress) > 0 {
		return false
	}

	return true
}

func (p *PrefixNode) SuperPrefix(prefixNode *PrefixNode) bool {
	//check if p.mask is longer
	if p.mask >= prefixNode.mask {
		return false
	}

	//check if p.minAddress > prefixNode.minAddress or p.maxAddress < prefixNode.maxAddress
	if bytes.Compare(p.minAddress, prefixNode.minAddress) > 0 || bytes.Compare(p.maxAddress, prefixNode.maxAddress) < 0 {
		return false
	}

	return true
}

func (p *PrefixNode) ValidAsNumber(asNumber uint32) bool {
	for _, asNum := range p.asNumbers {
		if asNum == asNumber {
			return true
		}
	}

	if p.parent != nil {
		return p.parent.ValidAsNumber(asNumber)
	} else {
		return false
	}
}

func (p *PrefixNode) Print(indent int) {
	for i := 0; i < indent; i++ {
		fmt.Printf("\t")
	}

	fmt.Printf("%s/%d : %v\n", p.ipAddress, p.mask, p.asNumbers)

	for _, child := range p.children {
		child.Print(indent + 1)
	}
}
