package topom

import (
	"math"
	"time"

	"github.com/wandoulabs/codis/pkg/models"
	"github.com/wandoulabs/codis/pkg/proxy"
	"github.com/wandoulabs/codis/pkg/utils/errors"
	"github.com/wandoulabs/codis/pkg/utils/log"
)

func (s *Topom) daemonRedisPool() {
	var ticker = time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-s.exit.C:
			return
		case <-ticker.C:
			s.redisp.Cleanup()
		}
	}
}

func (s *Topom) ListGroup() []*models.Group {
	s.mu.RLock()
	defer s.mu.RUnlock()
	glist := make([]*models.Group, 0, len(s.groups))
	for _, g := range s.groups {
		glist = append(glist, g)
	}
	return glist
}

func (s *Topom) getGroup(groupId int) (*models.Group, error) {
	if g := s.groups[groupId]; g != nil {
		return g, nil
	}
	return nil, errors.New("group does not exist")
}

func (s *Topom) getGroupMaster(groupId int) string {
	if g := s.groups[groupId]; g != nil && len(g.Servers) != 0 {
		return g.Servers[0]
	}
	return ""
}

func (s *Topom) CreateGroup(groupId int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosedTopom
	}

	if groupId <= 0 || groupId > math.MaxInt16 {
		return errors.New("invalid group id")
	}
	if s.groups[groupId] != nil {
		return errors.New("group already exists")
	}

	g := &models.Group{
		Id: groupId,
	}
	if err := s.store.CreateGroup(groupId, g); err != nil {
		log.WarnErrorf(err, "group-[%d] create failed", groupId)
		return errors.New("group create failed")
	}

	log.Infof("[%p] create group-[%d]", s, groupId)

	s.groups[groupId] = g
	return nil
}

func (s *Topom) RemoveGroup(groupId int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosedTopom
	}

	_, err := s.getGroup(groupId)
	if err != nil {
		return err
	}
	if len(s.getSlotsByGroup(groupId)) != 0 {
		return errors.New("group is still busy")
	}

	if err := s.store.RemoveGroup(groupId); err != nil {
		log.WarnErrorf(err, "group-[%d] remove failed", groupId)
		return errors.New("group remove failed")
	}

	log.Infof("[%p] remove group-[%d]", s, groupId)

	delete(s.groups, groupId)
	return nil
}

func (s *Topom) GroupAddNewServer(groupId int, addr string, force bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosedTopom
	}

	g, err := s.getGroup(groupId)
	if err != nil {
		return err
	}
	if g.Promoting {
		return errors.New("group is promoting")
	}

	for _, g := range s.groups {
		for _, x := range g.Servers {
			if x == addr {
				return errors.New("server already exists")
			}
		}
	}

	c, err := s.redisp.GetClient(addr)
	if err != nil {
		log.WarnErrorf(err, "group-[%d] open client failed, server = %s", groupId, addr)
		return errors.New("server open client failed")
	}
	defer s.redisp.PutClient(c)

	if _, err := c.SlotsInfo(); err != nil {
		log.WarnErrorf(err, "group-[%d] check codis-support failed, server = %s", groupId, addr)
		return errors.New("server check codis-supprot failed")
	}

	if !force {
		master, err := c.GetMaster()
		if err != nil {
			log.WarnErrorf(err, "group-[%d] verify master failed, server = %s", groupId, addr)
			return errors.New("server fetch master failed")
		}
		expect := s.getGroupMaster(groupId)
		if master != expect {
			log.Warnf("group-[%d] verify master failed, server = %s, master = %s, expect = %s", groupId, addr, master, expect)
			return errors.New("server check master failed")
		}
	}

	log.Infof("[%p] group-[%d] add server = %s", s, groupId, addr)

	servers := append(g.Servers, addr)

	n := &models.Group{
		Id:      groupId,
		Servers: servers,
	}
	if err := s.store.UpdateGroup(groupId, n); err != nil {
		log.WarnErrorf(err, "group-[%d] update failed", groupId)
		return errors.New("group update failed")
	}

	log.Infof("[%p] update group-[%d]: \n%s", s, groupId, n.ToJson())

	s.groups[groupId] = n
	return nil
}

func (s *Topom) GroupRemoveServer(groupId int, addr string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosedTopom
	}

	g, err := s.getGroup(groupId)
	if err != nil {
		return err
	}
	if g.Promoting {
		return errors.New("group is promoting")
	}

	servers := []string{}
	for _, x := range g.Servers {
		if x != addr {
			servers = append(servers, x)
		}
	}
	if len(g.Servers) == len(servers) {
		return errors.New("server does not exist")
	}
	if addr == g.Servers[0] {
		if len(g.Servers) != 1 || len(s.getSlotsByGroup(groupId)) != 0 {
			return errors.New("server is still busy")
		}
	}

	log.Infof("[%p] group-[%d] remove server = %s", s, groupId, addr)

	n := &models.Group{
		Id:      groupId,
		Servers: servers,
	}
	if err := s.store.UpdateGroup(groupId, n); err != nil {
		log.WarnErrorf(err, "group-[%d] update failed", groupId)
		return errors.New("group update failed")
	}

	log.Infof("[%p] update group-[%d]: \n%s", s, groupId, n.ToJson())

	s.groups[groupId] = n
	return nil
}

func (s *Topom) GroupPromoteServer(groupId int, addr string, force bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosedTopom
	}

	g, err := s.getGroup(groupId)
	if err != nil {
		return err
	}
	if g.Promoting {
		return errors.New("group is promoting")
	}

	servers := []string{}
	for _, x := range g.Servers {
		if x != addr {
			servers = append(servers, x)
		}
	}
	if len(g.Servers) == len(servers) {
		return errors.New("server does not exist")
	}
	if addr == g.Servers[0] {
		return errors.New("server is already master")
	}

	c, err := s.redisp.GetClient(addr)
	if err != nil {
		log.WarnErrorf(err, "group-[%d] open client failed, server = %s", groupId, addr)
		return errors.New("server open client failed")
	}
	defer s.redisp.PutClient(c)

	if !force {
		master, err := c.GetMaster()
		if err != nil {
			log.WarnErrorf(err, "group-[%d] verify master failed, server = %s", groupId, addr)
			return errors.New("server fetch master failed")
		}
		expect := s.getGroupMaster(groupId)
		if master != expect {
			log.Warnf("group-[%d] verify master failed, server = %s, master = %s, expect = %s", groupId, addr, master, expect)
			return errors.New("server check master failed")
		}
	}

	log.Infof("[%p] group-[%d] promote server = %s", s, groupId, addr)

	servers = append([]string{addr}, servers...)

	n := &models.Group{
		Id:        groupId,
		Servers:   servers,
		Promoting: true,
	}
	if err := s.store.UpdateGroup(groupId, n); err != nil {
		log.WarnErrorf(err, "group-[%d] update failed", groupId)
		return errors.New("group update failed")
	}

	log.Infof("[%p] update group: \n%s", s, n.ToJson())

	s.groups[groupId] = n
	return nil
}

func (s *Topom) GroupPromoteCommit(groupId int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosedTopom
	}

	g, err := s.getGroup(groupId)
	if err != nil {
		return err
	}
	if !g.Promoting {
		return errors.New("group is not promoting")
	}

	if errs := s.resyncGroup(groupId); len(errs) != 0 {
		log.Warnf("group-[%d] promote prepare failed", groupId)
		return errors.New("promote prepare failed")
	}

	n := &models.Group{
		Id:        groupId,
		Servers:   g.Servers,
		Promoting: false,
	}
	s.groups[groupId] = n

	if errs := s.resyncGroup(groupId); len(errs) != 0 {
		s.groups[groupId] = g
		log.Warnf("group-[%d] promote commit failed", groupId)
		return errors.New("promote commit failed")
	}

	if err := s.store.UpdateGroup(groupId, n); err != nil {
		s.groups[groupId] = g
		log.WarnErrorf(err, "group-[%d] update failed", groupId)
		return errors.New("group update failed")
	}

	log.Infof("[%p] update group: \n%s", s, n.ToJson())
	return nil
}

func (s *Topom) resyncGroup(groupId int) map[string]error {
	slots := s.getSlotsByGroup(groupId)
	if len(slots) == 0 {
		return map[string]error{}
	}
	return s.broadcast(func(p *models.Proxy, c *proxy.ApiClient) error {
		if err := c.FillSlots(slots...); err != nil {
			log.WarnErrorf(err, "proxy-[%s] resync group-[%d] failed", p.Token, groupId)
			return errors.New("proxy resync group failed")
		}
		return nil
	})
}

func (s *Topom) RepairGroup(groupId int, timeout int) error {
	s.mu.RLock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosedTopom
	}

	g, err := s.getGroup(groupId)
	if err != nil {
		return err
	}
	if g.Promoting {
		return errors.New("group is promoting")
	}

	var d time.Duration
	if timeout > 0 {
		d = time.Second * time.Duration(timeout)
	}

	for i, addr := range g.Servers {
		c, err := NewRedisClient(addr, s.config.ProductAuth, d)
		if err != nil {
			log.WarnErrorf(err, "group-[%d] open client failed, server = %s", groupId, addr)
			return errors.New("server open client failed")
		}
		defer c.Close()

		var master = ""
		if i != 0 {
			master = s.getGroupMaster(groupId)
		}
		if err := c.SlaveOf(master); err != nil {
			log.WarnErrorf(err, "group-[%d] set slaveof failed, server = %s", groupId, addr)
			return errors.New("server set slaveof failed")
		}
	}

	log.Infof("[%p] repair group-[%d]", s, groupId)
	return nil
}
