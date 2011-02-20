// Copyright 2010 Yves Junqueira
//
// Licensed under the Apache License, Version 2.0 (the "License"): you may
// not use this file except in compliance with the License. You may obtain
// a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS, WITHOUT
// WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied. See the
// License for the specific language governing permissions and limitations
// under the License.

package javaitarde

import (
	"flag"
	"fmt"
	"github.com/edsrzf/go-bson"
	"log"
	"os"
	"strconv"
	"strings"
)


var dryRunMode bool
var ignoredUsers string
var maxUnfollows int
var notifyUsers bool

type FollowersCrawler struct {
	ourUsers []int64
	db       *FollowersDatabase
	userMap  map[int64]string
	tw       *twitterClient
}

func NewFollowersCrawler() *FollowersCrawler {
	return &FollowersCrawler{
		tw:       newTwitterClient(),
		db:       NewFollowersDatabase(),
		ourUsers: make([]int64, 0),
		userMap:  map[int64]string{},
	}
}

func (c *FollowersCrawler) getUserName(uid int64) (screenName string, err os.Error) {
	// TODO: Save in our database.
	if screenName, ok := c.userMap[uid]; ok {
		return screenName, nil
	}
	if screenName, err = c.tw.getUserName(uid); err == nil {
		c.userMap[uid] = screenName
	}
	return
}


func (c *FollowersCrawler) saveUserFollowers(uf bson.Doc) (err os.Error) {
	if dryRunMode {
		return
	}
	if err = c.db.Insert(uf); err != nil {
		log.Println("Insert error", err.String())
	}
	return
}

func (c *FollowersCrawler) DiffFollowers(abandonedUser int64, prevUf, newUf bson.Doc) (unfollowers []int64) {
	unfollowers = make([]int64, 0)

	fOld, ok := prevUf["followers"]
	if !ok || fOld == nil {
		log.Printf("fOld: no followers %+v\n", fOld)
		return
	}
	fNew := newUf["followers"]
	if fNew == nil {
		log.Println("fNew: no followers")
		return
	}
	neww := map[int64]int{}
	for _, uid := range fNew.([]int64) {
		neww[uid] = 1
	}

	diff := len(fOld.([]interface{})) - len(neww)
	log.Printf("diff %d, max %d", diff, maxUnfollows)
	if diff > maxUnfollows {
		panic(fmt.Sprintf("too many unfollows %d > %d", diff, maxUnfollows))
	}

	// We don't care about new followers, only missing ones.
	for _, uid := range fOld.([]interface{}) {
		unfollower := uid.(int64)
		if unfollower < 184 {
			log.Println("ERROR while comparing user ", strconv.Itoa64(abandonedUser))
			log.Println("ERROR: bogus uid found in old database: ", unfollower)
			//panic("bogus uid" + strconv.Itoa64(uid.(int64)))
			c.db.Reconnect()
			continue
		}
		if _, ok := neww[unfollower]; !ok {
			if ignore, _ := strconv.Atoi64(ignoredUsers); ignore == unfollower {
				log.Println("(ignored)")
				continue
			}
			if unfollower == 118058049 {
				log.Println("ignored@@@@@@@@@@@@@@@")
				continue
			}
			unfollowers = append(unfollowers, unfollower)
		}
	}
	return
}

// Notify user and mark unfollow in the database.
func (c *FollowersCrawler) ProcessUnfollow(abandonedUser int64, unfollower int64) (err os.Error) {
	log.Printf("%v unfollowed by %v", abandonedUser, unfollower)
	if c.db.GetWasUnfollowNotified(abandonedUser, unfollower) {
		log.Println("already notified. ignoring")
		return
	}
	if err = c.NotifyUnfollower(abandonedUser, unfollower); err != nil {
		return err
	}
	if !dryRunMode {
		if err = c.db.MarkUnfollowNotified(abandonedUser, unfollower); err != nil {
			return err
		}
	}
	return
}

func (c *FollowersCrawler) NotifyUnfollower(abandonedUser, unfollower int64) (err os.Error) {
	abandonedName, err := c.getUserName(abandonedUser)
	if err != nil {
		log.Printf("c.getUserName(abandonedUser) err: %v", err)
		return
	}
	unfollowerName, err := c.getUserName(unfollower)
	if err != nil {
		log.Printf("c.getUserName(unfollower) err: %v", err)
		return
	}
	if dryRunMode || !notifyUsers {
		return
	}
	return c.tw.NotifyUnfollower(abandonedName, unfollowerName)
}

func (c *FollowersCrawler) FollowUser(uid int64) (err os.Error) {
	if dryRunMode {
		return
	}
	if isPending, _ := c.db.GetIsFollowingPending(uid); isPending {
		log.Println("Already trying to follow user. Skipping follow request.")
		return
	}
	if err = c.tw.FollowUser(uid); err == nil {
		c.db.MarkPendingFollow(uid)
	}
	return
}

func (c *FollowersCrawler) GetAllUsersFollowers() (err os.Error) {
	for _, u := range c.ourUsers {
		prevUf := bson.Doc{}
		newUf := bson.Doc{}
		if prevUf, err = c.db.GetUserFollowers(u); err != nil {
			log.Printf("db.GetUserFollowers err=%s, userId=%d\n", err.String(), u)
			prevUf = nil
		}
		if newUf, err = c.tw.getUserFollowers(u, ""); err != nil {
			if strings.Contains(err.String(), " 401") {
				// User's follower list is blocked. Need to request access.
				c.FollowUser(u)
			} else {
				log.Printf("TwitterGetUserFollowers err=%s, userId=%d\n", err.String(), u)
			}
			newUf = nil
		}
		if prevUf != nil && newUf != nil {
			for _, unfollower := range c.DiffFollowers(u, prevUf, newUf) {
				if err := c.ProcessUnfollow(u, unfollower); err != nil {
					log.Printf("ProcessUnfollow failure, userId=%d, unfollower=%v. Err: %v", u, unfollower, err)
				}
			}
			if err := c.saveUserFollowers(newUf); err != nil {
				log.Printf("c.saveUserFollowers(), u=%v, err=%v", u, err)
			}
		}

	}
	return
}

// Find everyone who follows us, so we know who to crawl.
func (c *FollowersCrawler) FindOurUsers(uid int64) (err os.Error) {
	userFollowers, err := c.tw.getUserFollowers(uid, "")
	if err != nil {
		return err
	}
	if err := c.saveUserFollowers(userFollowers); err != nil {
		log.Printf("c.saveUserFollowers(), u=%v, err=%v", uid, err)
	}
	c.ourUsers = userFollowers["followers"].([]int64)
	return
}

func (c *FollowersCrawler) TestStuff() {
	log.Println("gogo")
	u, _ := c.db.GetUserFollowers(16196534)
	for f1, f2 := range u["followers"].([]interface{}) {
		if f2.(int64) == 118058049 {
			log.Println(f1, f2)
		}
		if f2.(int64) < 1000 {
			log.Println(f1, f2)
		}
	}
}

func init() {
	flag.BoolVar(&dryRunMode, "dryrun", true,
		"Don't make changes to the database.")
	flag.BoolVar(&notifyUsers, "notifyUsers", true,
		"Notify unfollows to users.")
	flag.IntVar(&maxUnfollows, "maxUnfollows", 50, "Panic if the number of unfollows for a user exceeds this.")
	// TODO(nictuku): Make this a list.
	flag.StringVar(&ignoredUsers, "ignoreUsers", "118058049",
		"UserID to ignore (flaky twitter results)")
}
