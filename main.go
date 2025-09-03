package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

type MenuItem struct {
	ID          uint      `json:"id" gorm:"primaryKey"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Price       float64   `json:"price"`
	MealType    string    `json:"meal_type"` // weekday-breakfast, weekday-lunch, weekend-breakfast, weekend-lunch
	ImageURL    string    `json:"image_url"`
	CreatedAt   time.Time `json:"created_at"`
}

type Subscriber struct {
	ID        uint      `json:"id" gorm:"primaryKey"`
	Name      string    `json:"name"`
	Email     string    `json:"email"`
	Plan      string    `json:"plan"`
	StartDate time.Time `json:"start_date"`
	EndDate   time.Time `json:"end_date"`
	Active    bool      `json:"active"`
	CreatedAt time.Time `json:"created_at"`
}

type Order struct {
	ID           uint      `json:"id" gorm:"primaryKey"`
	SubscriberID uint      `json:"subscriber_id"`
	MenuItemID   uint      `json:"menu_item_id"`
	Date         time.Time `json:"date"`
}

var db *gorm.DB

func main() {
	var err error
	db, err = gorm.Open(sqlite.Open("joevis.db"), &gorm.Config{})
	if err != nil {
		log.Fatal("failed to connect db:", err)
	}

	// migrations
	if err := db.AutoMigrate(&MenuItem{}, &Subscriber{}, &Order{}); err != nil {
		log.Fatal(err)
	}

	// create uploads dir
	os.MkdirAll("uploads", 0755)

	r := gin.Default()

	// allow requests from Expo / phone
	r.Use(cors.New(cors.Config{
		AllowOrigins:     []string{"*"},
		AllowMethods:     []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"},
		AllowHeaders:     []string{"Origin", "Content-Type", "Authorization"},
		AllowCredentials: true,
		MaxAge:           12 * time.Hour,
	}))

	// public endpoints
	r.GET("/health", health)
	r.GET("/menus", getMenus)
	r.GET("/menus/:id", getMenu)
	r.POST("/subscribe", subscribe)
	r.GET("/analytics", analytics)           // aggregated but safe for admin; protected by password in query for simplicity
	r.GET("/recommendations", recommendations)
	r.Static("/uploads", "./uploads")

	// admin endpoints (simple password check via header x-admin-pw)
	admin := r.Group("/admin", adminAuth())
	{
		admin.POST("/menu", addMenu)
		admin.PUT("/menu/:id", editMenu)
		admin.DELETE("/menu/:id", deleteMenu)
		admin.POST("/upload", uploadImage)
		admin.GET("/subscribers", listSubscribers)
	}

	// optionally seed if empty
	seedIfEmpty()

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	log.Println("starting backend on port", port)
	r.Run(":" + port)
}

func health(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

func getMenus(c *gin.Context) {
	var items []MenuItem
	// optional meal filter
	mealType := c.Query("meal_type")
	if mealType != "" {
		db.Where("meal_type = ?", mealType).Order("created_at desc").Find(&items)
	} else {
		db.Order("created_at desc").Find(&items)
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
}

func getMenu(c *gin.Context) {
	id := c.Param("id")
	var item MenuItem
	if err := db.First(&item, id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}
	c.JSON(http.StatusOK, item)
}

type SubscribePayload struct {
	Name  string `json:"name" binding:"required"`
	Email string `json:"email" binding:"required,email"`
	Plan  string `json:"plan" binding:"required"`
}

func subscribe(c *gin.Context) {
	var p SubscribePayload
	if err := c.ShouldBindJSON(&p); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	now := time.Now().UTC()
	sub := Subscriber{
		Name:      p.Name,
		Email:     p.Email,
		Plan:      p.Plan,
		StartDate: now,
		EndDate:   now.AddDate(0, 0, 30),
		Active:    true,
	}
	if err := db.Create(&sub).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to save subscriber"})
		return
	}
	c.JSON(http.StatusCreated, gin.H{"ok": true, "subscriber": sub})
}

func recommendations(c *gin.Context) {
	// simple popularity: join orders grouped by item
	type Res struct {
		MenuItemID uint
		Cnt        int
	}
	var rows []Res
	db.Raw("SELECT menu_item_id, COUNT(*) as cnt FROM orders GROUP BY menu_item_id ORDER BY cnt DESC LIMIT 5").Scan(&rows)
	var ids []uint
	for _, r := range rows {
		ids = append(ids, r.MenuItemID)
	}
	var items []MenuItem
	if len(ids) > 0 {
		db.Where("id IN ?", ids).Find(&items)
	} else {
		db.Order("price asc").Limit(5).Find(&items)
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
}

func analytics(c *gin.Context) {
	// basic analytics; require admin pw for full data
	adminPW := c.GetHeader("x-admin-pw")
	total := int64(0)
	active := int64(0)
	db.Model(&Subscriber{}).Count(&total)
	db.Model(&Subscriber{}).Where("active = ?", true).Count(&active)

	// top menu items
	type Top struct {
		MenuItemID uint
		Count      int
	}
	var top []Top
	db.Raw("SELECT menu_item_id, COUNT(*) as count FROM orders GROUP BY menu_item_id ORDER BY count DESC LIMIT 6").Scan(&top)

	var topItems []map[string]interface{}
	for _, t := range top {
		var mi MenuItem
		if err := db.First(&mi, t.MenuItemID).Error; err == nil {
			topItems = append(topItems, map[string]interface{}{
				"name":  mi.Name,
				"count": t.Count,
			})
		}
	}

	result := gin.H{
		"total_subscribers": total,
		"active_subscribers": active,
		"top_items":         topItems,
	}
	// include list of recent subscribers only for admin
	if adminPW != "" && adminPW == os.Getenv("ADMIN_PASSWORD") {
		var subs []Subscriber
		db.Order("created_at desc").Limit(10).Find(&subs)
		result["recent_subscribers"] = subs
	}
	c.JSON(http.StatusOK, result)
}

func seedIfEmpty() {
	var cnt int64
	db.Model(&MenuItem{}).Count(&cnt)
	if cnt == 0 {
		items := []MenuItem{
			{Name: "Akamu & Akara", Description: "Smooth pap and flaky akara", Price: 600, MealType: "weekday-breakfast"},
			{Name: "Moi Moi Deluxe", Description: "Steamed bean pudding", Price: 900, MealType: "weekday-breakfast"},
			{Name: "Jollof Rice & Chicken", Description: "Signature jollof with chicken", Price: 1500, MealType: "weekday-lunch"},
			{Name: "Gizdodo Bowl", Description: "Gizzard and plantain", Price: 1300, MealType: "weekday-lunch"},
			{Name: "Weekend Egusi", Description: "Rich egusi for the weekend", Price: 1800, MealType: "weekend-lunch"},
		}
		for _, it := range items {
			db.Create(&it)
		}
	}
}

func uploadImage(c *gin.Context) {
	file, err := c.FormFile("file")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "no file"})
		return
	}
	ext := filepath.Ext(file.Filename)
	name := fmt.Sprintf("%d%s", time.Now().UnixNano(), ext)
	path := filepath.Join("uploads", name)
	if err := c.SaveUploadedFile(file, path); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "save failed"})
		return
	}
	url := "/uploads/" + name
	c.JSON(http.StatusOK, gin.H{"url": url})
}

func addMenu(c *gin.Context) {
	var m MenuItem
	if err := c.ShouldBindJSON(&m); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := db.Create(&m).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "create failed"})
		return
	}
	c.JSON(http.StatusCreated, m)
}

func editMenu(c *gin.Context) {
	id := c.Param("id")
	var m MenuItem
	if err := db.First(&m, id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}
	var payload MenuItem
	if err := c.ShouldBindJSON(&payload); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	m.Name = payload.Name
	m.Description = payload.Description
	m.Price = payload.Price
	m.MealType = payload.MealType
	if payload.ImageURL != "" {
		m.ImageURL = payload.ImageURL
	}
	db.Save(&m)
	c.JSON(http.StatusOK, m)
}

func deleteMenu(c *gin.Context) {
	id := c.Param("id")
	if err := db.Delete(&MenuItem{}, id).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "delete failed"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func listSubscribers(c *gin.Context) {
	var subs []Subscriber
	db.Order("created_at desc").Find(&subs)
	c.JSON(http.StatusOK, subs)
}

// adminAuth middleware checks x-admin-pw header against ADMIN_PASSWORD env var
func adminAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		pw := c.GetHeader("x-admin-pw")
		if pw == "" || pw != os.Getenv("ADMIN_PASSWORD") {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
			return
		}
		c.Next()
	}
}