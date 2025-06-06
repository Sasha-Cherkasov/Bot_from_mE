package main

import (
	"encoding/csv"
	"fmt"
	"log"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/joho/godotenv"
)

const (
	managerPhone     = "ТУТ НОМЕР БУДЕТ ХОСТЕС"
	adminChatID      = 5069516411
	reservationsFile = "reservations.csv"
	timeZone         = "Europe/Moscow"
	minBookingHours  = 2
	reservationTTL   = 15 * time.Minute
)

const (
	stateMainMenu = iota
	stateWaitingForName
	stateWaitingForPhone
	stateWaitingForManualPhone
	stateWaitingForGuests
	stateWaitingForDate
	stateWaitingForTime
	stateWaitingForComment
	stateEditingReservation
	stateEditingReservationName
	stateEditingReservationPhone
	stateEditingReservationGuests
	stateEditingReservationDate
	stateEditingReservationTime
	stateEditingReservationComment
)

type Reservation struct {
	ID        string
	ChatID    int64
	Name      string
	Phone     string
	Guests    int
	Date      string
	Time      string
	Comment   string
	Confirmed bool
	CreatedAt time.Time
}

type UserState struct {
	State           int
	Name            string
	PhoneContact    string
	PhoneManual     string
	Guests          int
	Date            string
	Comment         string
	TempReservation *Reservation
}

var (
	userStates   = make(map[int64]UserState)
	reservations = make(map[string]Reservation)
	phoneRegex   = regexp.MustCompile(`^[\d]{11}$`)
	loc, _       = time.LoadLocation(timeZone)
)

func main() {
	if err := godotenv.Load(); err != nil {
		log.Print("Файл .env не найден")
	}

	botToken := os.Getenv("TELEGRAM_BOT_TOKEN")
	if botToken == "" {
		log.Panic("Токен бота не установлен")
	}

	bot, err := tgbotapi.NewBotAPI(botToken)
	if err != nil {
		log.Panic("Ошибка создания бота:", err)
	}

	bot.Debug = true
	log.Printf("Авторизован как %s", bot.Self.UserName)

	initReservationsFile()
	loadReservationsFromFile()

	_, _ = bot.Request(tgbotapi.DeleteWebhookConfig{})

	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60
	updates := bot.GetUpdatesChan(u)

	go cleanupExpiredReservations(bot)

	for update := range updates {
		if update.Message != nil {
			handleMessage(bot, update.Message)
		} else if update.CallbackQuery != nil {
			handleCallbackQuery(bot, update.CallbackQuery)
		}
	}
}

func initReservationsFile() {
	if _, err := os.Stat(reservationsFile); os.IsNotExist(err) {
		file, err := os.Create(reservationsFile)
		if err != nil {
			log.Printf("Ошибка создания файла бронирований: %v", err)
			return
		}
		defer file.Close()

		writer := csv.NewWriter(file)
		headers := []string{
			"ID",
			"ChatID",
			"Name",
			"Phone",
			"Guests",
			"Date",
			"Time",
			"Comment",
			"Confirmed",
			"CreatedAt",
		}
		writer.Write(headers)
		writer.Flush()
	}
}

func cleanupExpiredReservations(bot *tgbotapi.BotAPI) {
	for {
		currentTime := time.Now().In(loc)
		for id, r := range reservations {
			reservationTime, err := time.ParseInLocation("02.01.2006 15:04", r.Date+" "+r.Time, loc)
			if err != nil {
				continue
			}

			if currentTime.After(reservationTime.Add(reservationTTL)) {
				delete(reservations, id)
				deleteReservationFromFile(id)
				log.Printf("Бронь %s удалена (истек срок)", id)
			}
		}
		time.Sleep(5 * time.Minute)
	}
}

func loadReservationsFromFile() {
	file, err := os.Open(reservationsFile)
	if err != nil {
		if os.IsNotExist(err) {
			return
		}
		log.Printf("Ошибка при открытии файла бронирований: %v", err)
		return
	}
	defer file.Close()

	reader := csv.NewReader(file)
	reader.Comma = ','
	reader.FieldsPerRecord = -1

	if _, err := reader.Read(); err != nil {
		log.Printf("Ошибка чтения заголовка: %v", err)
		return
	}

	records, err := reader.ReadAll()
	if err != nil {
		log.Printf("Ошибка чтения файла бронирований: %v", err)
		return
	}

	for _, record := range records {
		if len(record) < 10 {
			continue
		}

		chatID, err := strconv.ParseInt(record[1], 10, 64)
		if err != nil {
			log.Printf("Ошибка парсинга ChatID: %v", err)
			continue
		}

		name := record[2]
		if name == "" {
			log.Printf("Пустое имя в брони ID: %s", record[0])
			continue
		}

		guests, err := strconv.Atoi(record[4])
		if err != nil {
			log.Printf("Ошибка парсинга количества гостей: %v", err)
			continue
		}

		confirmed, err := strconv.ParseBool(record[8])
		if err != nil {
			log.Printf("Ошибка парсинга статуса подтверждения: %v", err)
			continue
		}

		createdAt, err := time.Parse(time.RFC3339, record[9])
		if err != nil {
			log.Printf("Ошибка парсинга даты создания: %v", err)
			continue
		}

		reservation := Reservation{
			ID:        record[0],
			ChatID:    chatID,
			Name:      name,
			Phone:     record[3],
			Guests:    guests,
			Date:      record[5],
			Time:      record[6],
			Comment:   record[7],
			Confirmed: confirmed,
			CreatedAt: createdAt,
		}

		reservations[reservation.ID] = reservation
		log.Printf("Загружена бронь: ID=%s, Имя='%s'", reservation.ID, reservation.Name)
	}
}

func clearUserState(chatID int64) {
	userStates[chatID] = UserState{State: stateMainMenu}
}

func handleMessage(bot *tgbotapi.BotAPI, message *tgbotapi.Message) {
	chatID := message.Chat.ID
	state, exists := userStates[chatID]

	if message.Contact != nil && state.State == stateWaitingForPhone {
		phone := normalizePhone(message.Contact.PhoneNumber)
		if !phoneRegex.MatchString(phone) {
			sendMessage(bot, chatID, "Номер телефона должен содержать 11 цифр. Пожалуйста, проверьте правильность написания.", true)
			return
		}
		userStates[chatID] = UserState{
			State:           stateWaitingForGuests,
			Name:            state.Name,
			PhoneContact:    phone,
			PhoneManual:     state.PhoneManual,
			Guests:          state.Guests,
			Date:            state.Date,
			Comment:         state.Comment,
			TempReservation: state.TempReservation,
		}
		log.Printf("Сохранен контактный телефон для chatID %d: Имя='%s', Телефон='%s'", chatID, state.Name, phone)
		sendMessage(bot, chatID, "Спасибо! Теперь укажите количество гостей:", true)
		return
	}

	switch message.Text {
	case "/start":
		clearUserState(chatID)
		showMainMenu(bot, chatID, hasActiveReservations(chatID))
		return
	case "Забронировать стол":
		clearUserState(chatID)
		askForName(bot, chatID)
		return
	case "Связаться с нами":
		sendMessage(bot, chatID, "Наш телефон для связи: "+managerPhone, false)
		return
	case "Моя бронь":
		clearUserState(chatID)
		showUserReservations(bot, chatID)
		return
	case "Назад":
		clearUserState(chatID)
		showMainMenuSilent(bot, chatID, hasActiveReservations(chatID))
		return
	case "Пропустить":
		if state.State == stateWaitingForComment {
			userStates[chatID] = UserState{
				State:           stateWaitingForDate,
				Name:            state.Name,
				PhoneContact:    state.PhoneContact,
				PhoneManual:     state.PhoneManual,
				Guests:          state.Guests,
				Date:            state.Date,
				Comment:         "-",
				TempReservation: state.TempReservation,
			}
			log.Printf("Пропущен комментарий для chatID %d", chatID)
			askForDate(bot, chatID)
			return
		}
	}

	if exists {
		switch state.State {
		case stateWaitingForName:
			name := strings.TrimSpace(message.Text)
			if len(name) < 2 {
				sendMessage(bot, chatID, "Имя должно содержать хотя бы 2 символа. Пожалуйста, введите ваше имя:", true)
				return
			}
			userStates[chatID] = UserState{
				State:           stateWaitingForPhone,
				Name:            name,
				PhoneContact:    state.PhoneContact,
				PhoneManual:     state.PhoneManual,
				Guests:          state.Guests,
				Date:            state.Date,
				Comment:         state.Comment,
				TempReservation: state.TempReservation,
			}
			log.Printf("Сохранено имя для chatID %d: '%s'", chatID, name)
			askForPhone(bot, chatID)
			return
		case stateWaitingForManualPhone:
			phone := normalizePhone(message.Text)
			if !phoneRegex.MatchString(phone) {
				sendMessage(bot, chatID, "Номер телефона должен содержать 11 цифр. Пожалуйста, проверьте правильность написания.", true)
				return
			}
			userStates[chatID] = UserState{
				State:           stateWaitingForGuests,
				Name:            state.Name,
				PhoneContact:    state.PhoneContact,
				PhoneManual:     phone,
				Guests:          state.Guests,
				Date:            state.Date,
				Comment:         state.Comment,
				TempReservation: state.TempReservation,
			}
			log.Printf("Сохранен ручной телефон для chatID %d: Имя='%s', Телефон='%s'", chatID, state.Name, phone)
			sendMessage(bot, chatID, "Спасибо! Теперь укажите количество гостей:", true)
			return
		case stateWaitingForGuests:
			guests, err := strconv.Atoi(message.Text)
			if err != nil || guests <= 0 {
				sendMessage(bot, chatID, "Пожалуйста, введите корректное количество гостей (число больше 0).", true)
				return
			}
			userStates[chatID] = UserState{
				State:           stateWaitingForComment,
				Name:            state.Name,
				PhoneContact:    state.PhoneContact,
				PhoneManual:     state.PhoneManual,
				Guests:          guests,
				Date:            state.Date,
				Comment:         state.Comment,
				TempReservation: state.TempReservation,
			}
			log.Printf("Сохранено количество гостей для chatID %d: %d", chatID, guests)
			askForComment(bot, chatID)
			return
		case stateWaitingForComment:
			comment := strings.TrimSpace(message.Text)
			if comment == "" {
				comment = "-"
			}
			userStates[chatID] = UserState{
				State:           stateWaitingForDate,
				Name:            state.Name,
				PhoneContact:    state.PhoneContact,
				PhoneManual:     state.PhoneManual,
				Guests:          state.Guests,
				Date:            state.Date,
				Comment:         comment,
				TempReservation: state.TempReservation,
			}
			log.Printf("Сохранен комментарий для chatID %d: '%s'", chatID, comment)
			askForDate(bot, chatID)
			return
		case stateEditingReservationName:
			name := strings.TrimSpace(message.Text)
			if len(name) < 2 {
				sendMessage(bot, chatID, "Имя должно содержать хотя бы 2 символа. Пожалуйста, введите ваше имя:", true)
				return
			}
			if state.TempReservation == nil {
				sendMessage(bot, chatID, "Ошибка редактирования. Пожалуйста, начните заново.", false)
				showMainMenu(bot, chatID, hasActiveReservations(chatID))
				return
			}
			state.TempReservation.Name = name
			userStates[chatID] = state
			showEditOptions(bot, chatID, *state.TempReservation)
			return
		case stateEditingReservationPhone:
			phone := normalizePhone(message.Text)
			if !phoneRegex.MatchString(phone) {
				sendMessage(bot, chatID, "Номер телефона должен содержать 11 цифр. Пожалуйста, проверьте правильность написания.", true)
				return
			}
			if state.TempReservation == nil {
				sendMessage(bot, chatID, "Ошибка редактирования. Пожалуйста, начните заново.", false)
				showMainMenu(bot, chatID, hasActiveReservations(chatID))
				return
			}
			state.TempReservation.Phone = phone
			userStates[chatID] = state
			showEditOptions(bot, chatID, *state.TempReservation)
			return
		case stateEditingReservationGuests:
			guests, err := strconv.Atoi(message.Text)
			if err != nil || guests <= 0 {
				sendMessage(bot, chatID, "Пожалуйста, введите корректное количество гостей (число больше 0).", true)
				return
			}
			if state.TempReservation == nil {
				sendMessage(bot, chatID, "Ошибка редактирования. Пожалуйста, начните заново.", false)
				showMainMenu(bot, chatID, hasActiveReservations(chatID))
				return
			}
			state.TempReservation.Guests = guests
			userStates[chatID] = state
			showEditOptions(bot, chatID, *state.TempReservation)
			return
		case stateEditingReservationDate:
			date := strings.TrimSpace(message.Text)
			_, err := time.ParseInLocation("02.01.2006", date, loc)
			if err != nil {
				sendMessage(bot, chatID, "Пожалуйста, введите дату в формате ДД.ММ.ГГГГ.", true)
				return
			}
			if state.TempReservation == nil {
				sendMessage(bot, chatID, "Ошибка редактирования. Пожалуйста, начните заново.", false)
				showMainMenu(bot, chatID, hasActiveReservations(chatID))
				return
			}
			state.TempReservation.Date = date
			userStates[chatID] = state
			askForTime(bot, chatID)
			return
		case stateEditingReservationTime:
			timeStr := strings.TrimSpace(message.Text)
			_, err := time.ParseInLocation("15:04", timeStr, loc)
			if err != nil {
				sendMessage(bot, chatID, "Пожалуйста, введите время в формате ЧЧ:ММ.", true)
				return
			}
			if state.TempReservation == nil {
				sendMessage(bot, chatID, "Ошибка редактирования. Пожалуйста, начните заново.", false)
				showMainMenu(bot, chatID, hasActiveReservations(chatID))
				return
			}
			state.TempReservation.Time = timeStr
			userStates[chatID] = state
			showEditOptions(bot, chatID, *state.TempReservation)
			return
		case stateEditingReservationComment:
			comment := strings.TrimSpace(message.Text)
			if comment == "" {
				comment = "-"
			}
			if state.TempReservation == nil {
				sendMessage(bot, chatID, "Ошибка редактирования. Пожалуйста, начните заново.", false)
				showMainMenu(bot, chatID, hasActiveReservations(chatID))
				return
			}
			state.TempReservation.Comment = comment
			userStates[chatID] = state
			showEditOptions(bot, chatID, *state.TempReservation)
			return
		}
	}

	showMainMenu(bot, chatID, hasActiveReservations(chatID))
}

func showMainMenu(bot *tgbotapi.BotAPI, chatID int64, showMyReservationButton bool) {
	state, exists := userStates[chatID]
	if !exists {
		state = UserState{State: stateMainMenu}
	} else {
		state.State = stateMainMenu
	}
	userStates[chatID] = state

	buttons := []tgbotapi.KeyboardButton{
		tgbotapi.NewKeyboardButton("Забронировать стол"),
		tgbotapi.NewKeyboardButton("Связаться с нами"),
	}

	if showMyReservationButton {
		buttons = append(buttons, tgbotapi.NewKeyboardButton("Моя бронь"))
	}

	var keyboardRows [][]tgbotapi.KeyboardButton
	keyboardRows = append(keyboardRows, buttons[:2])
	if len(buttons) > 2 {
		keyboardRows = append(keyboardRows, buttons[2:])
	}

	msg := tgbotapi.NewMessage(chatID, "Выберите действие:")
	msg.ReplyMarkup = tgbotapi.NewReplyKeyboard(keyboardRows...)
	bot.Send(msg)
}

func showMainMenuSilent(bot *tgbotapi.BotAPI, chatID int64, showMyReservationButton bool) {
	state, exists := userStates[chatID]
	if !exists {
		state = UserState{State: stateMainMenu}
	} else {
		state.State = stateMainMenu
	}
	userStates[chatID] = state

	buttons := []tgbotapi.KeyboardButton{
		tgbotapi.NewKeyboardButton("Забронировать стол"),
		tgbotapi.NewKeyboardButton("Связаться с нами"),
	}

	if showMyReservationButton {
		buttons = append(buttons, tgbotapi.NewKeyboardButton("Моя бронь"))
	}

	var keyboardRows [][]tgbotapi.KeyboardButton
	keyboardRows = append(keyboardRows, buttons[:2])
	if len(buttons) > 2 {
		keyboardRows = append(keyboardRows, buttons[2:])
	}

	msg := tgbotapi.NewMessage(chatID, "")
	msg.ReplyMarkup = tgbotapi.NewReplyKeyboard(keyboardRows...)
	bot.Send(msg)
}

func askForName(bot *tgbotapi.BotAPI, chatID int64) {
	sendMessage(bot, chatID, "Пожалуйста, введите ваше имя:", true)
	userStates[chatID] = UserState{State: stateWaitingForName}
}

func askForPhone(bot *tgbotapi.BotAPI, chatID int64) {
	msg := tgbotapi.NewMessage(chatID, "Как вы хотите предоставить номер телефона?")
	buttons := [][]tgbotapi.InlineKeyboardButton{
		{tgbotapi.NewInlineKeyboardButtonData("📲 Поделиться контактом", "phone_contact")},
		{tgbotapi.NewInlineKeyboardButtonData("⌨ Ввести вручную", "phone_manual")},
		{tgbotapi.NewInlineKeyboardButtonData("❌ Отмена", "cancel")},
	}
	msg.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(buttons...)
	bot.Send(msg)
}

func askForDate(bot *tgbotapi.BotAPI, chatID int64) {
	msg := tgbotapi.NewMessage(chatID, "Выберите дату бронирования:")
	var buttons [][]tgbotapi.InlineKeyboardButton
	var row []tgbotapi.InlineKeyboardButton

	today := time.Now().In(loc)
	for i := 0; i < 10; i++ {
		date := today.AddDate(0, 0, i)
		dateStr := date.Format("02.01.2006")
		row = append(row, tgbotapi.NewInlineKeyboardButtonData(dateStr, "date_"+dateStr))
		if len(row) == 4 || i == 9 {
			buttons = append(buttons, row)
			row = []tgbotapi.InlineKeyboardButton{}
		}
	}

	buttons = append(buttons, []tgbotapi.InlineKeyboardButton{
		tgbotapi.NewInlineKeyboardButtonData("❌ Отмена", "cancel"),
	})

	msg.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(buttons...)
	bot.Send(msg)
}

func askForTime(bot *tgbotapi.BotAPI, chatID int64) {
	msg := tgbotapi.NewMessage(chatID, "Выберите время бронирования:")
	var buttons [][]tgbotapi.InlineKeyboardButton
	var row []tgbotapi.InlineKeyboardButton

	now := time.Now().In(loc)
	selectedDate := userStates[chatID].Date

	minBookingTime := now.Add(time.Hour * minBookingHours)
	minHour := minBookingTime.Hour()
	minMinute := minBookingTime.Minute()

	count := 0
	for hour := 16; hour <= 23; hour++ {
		for minute := 0; minute <= 30; minute += 30 {
			if selectedDate == now.Format("02.01.2006") {
				if hour < minHour || (hour == minHour && minute < minMinute) {
					continue
				}
			}
			timeStr := fmt.Sprintf("%02d:%02d", hour, minute)
			row = append(row, tgbotapi.NewInlineKeyboardButtonData(timeStr, "time_"+timeStr))
			count++
			if count%4 == 0 || (hour == 23 && minute == 30) {
				buttons = append(buttons, row)
				row = []tgbotapi.InlineKeyboardButton{}
			}
		}
	}

	buttons = append(buttons, []tgbotapi.InlineKeyboardButton{
		tgbotapi.NewInlineKeyboardButtonData("❌ Отмена", "cancel"),
	})

	msg.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(buttons...)
	bot.Send(msg)
}

func askForComment(bot *tgbotapi.BotAPI, chatID int64) {
	msg := tgbotapi.NewMessage(chatID, "Укажите ваши пожелания или комментарий к брони:")
	msg.ReplyMarkup = tgbotapi.NewReplyKeyboard(
		tgbotapi.NewKeyboardButtonRow(
			tgbotapi.NewKeyboardButton("Пропустить"),
		),
	)
	bot.Send(msg)
}

func showUserReservations(bot *tgbotapi.BotAPI, chatID int64) {
	userReservations := getUserActiveReservations(chatID)

	if len(userReservations) == 0 {
		sendMessage(bot, chatID, "У вас нет активных бронирований.", false)
		showMainMenu(bot, chatID, false)
		return
	}

	for _, r := range userReservations {
		msgText := fmt.Sprintf(
			"Бронь #%s\n\nИмя: %s\nТелефон: %s\nГостей: %d\nДата: %s\nВремя: %s",
			r.ID, r.Name, r.Phone, r.Guests, r.Date, r.Time)

		if r.Comment != "" && r.Comment != "-" {
			msgText += fmt.Sprintf("\nКомментарий: %s", r.Comment)
		}

		msg := tgbotapi.NewMessage(chatID, msgText)
		buttons := tgbotapi.NewInlineKeyboardMarkup(
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("Редактировать", "edit_select_"+r.ID),
				tgbotapi.NewInlineKeyboardButtonData("Удалить", "edit_delete_"+r.ID),
			),
		)
		msg.ReplyMarkup = buttons
		bot.Send(msg)
	}

	msg := tgbotapi.NewMessage(chatID, "")
	msg.ReplyMarkup = tgbotapi.NewReplyKeyboard(
		tgbotapi.NewKeyboardButtonRow(
			tgbotapi.NewKeyboardButton("Назад"),
			tgbotapi.NewKeyboardButton("Забронировать стол"),
		),
		tgbotapi.NewKeyboardButtonRow(
			tgbotapi.NewKeyboardButton("Связаться с нами"),
		),
	)
	bot.Send(msg)
}

func getUserActiveReservations(chatID int64) []Reservation {
	var activeReservations []Reservation
	now := time.Now().In(loc)

	for _, r := range reservations {
		if r.ChatID == chatID && r.Confirmed {
			reservationTime, err := time.ParseInLocation("02.01.2006 15:04", r.Date+" "+r.Time, loc)
			if err != nil {
				continue
			}

			if now.Before(reservationTime.Add(reservationTTL)) {
				activeReservations = append(activeReservations, r)
			}
		}
	}
	return activeReservations
}

func hasActiveReservations(chatID int64) bool {
	now := time.Now().In(loc)
	for _, r := range reservations {
		if r.ChatID == chatID && r.Confirmed {
			reservationTime, err := time.ParseInLocation("02.01.2006 15:04", r.Date+" "+r.Time, loc)
			if err != nil {
				continue
			}

			if now.Before(reservationTime.Add(reservationTTL)) {
				return true
			}
		}
	}
	return false
}

func normalizePhone(phone string) string {
	re := regexp.MustCompile(`\D`)
	return re.ReplaceAllString(phone, "")
}

func handleCallbackQuery(bot *tgbotapi.BotAPI, query *tgbotapi.CallbackQuery) {
	chatID := query.Message.Chat.ID
	data := query.Data

	callback := tgbotapi.NewCallback(query.ID, "")
	if _, err := bot.Request(callback); err != nil {
		log.Println("Ошибка callback:", err)
	}

	if strings.HasPrefix(data, "time_") {
		selectedTime := strings.TrimPrefix(data, "time_")
		processTimeSelection(bot, chatID, selectedTime)
		return
	}

	if strings.HasPrefix(data, "date_") {
		selectedDate := strings.TrimPrefix(data, "date_")
		processDateSelection(bot, chatID, selectedDate)
		return
	}

	if strings.HasPrefix(data, "edit_") {
		action := strings.TrimPrefix(data, "edit_")
		handleEditAction(bot, chatID, action)
		return
	}

	switch data {
	case "phone_contact":
		requestContact(bot, chatID)
	case "phone_manual":
		sendMessage(bot, chatID, "Пожалуйста, введите ваш номер телефона (11 цифр):", true)
		userStates[chatID] = UserState{
			State:           stateWaitingForManualPhone,
			Name:            userStates[chatID].Name,
			PhoneContact:    userStates[chatID].PhoneContact,
			PhoneManual:     userStates[chatID].PhoneManual,
			Guests:          userStates[chatID].Guests,
			Date:            userStates[chatID].Date,
			Comment:         userStates[chatID].Comment,
			TempReservation: userStates[chatID].TempReservation,
		}
	case "cancel":
		clearUserState(chatID)
		showMainMenu(bot, chatID, hasActiveReservations(chatID))
	}
}

func requestContact(bot *tgbotapi.BotAPI, chatID int64) {
	msg := tgbotapi.NewMessage(chatID, "Нажмите кнопку ниже, чтобы поделиться контактом:")
	contactBtn := tgbotapi.NewKeyboardButtonContact("📲 Отправить мой контакт")
	keyboard := tgbotapi.NewReplyKeyboard(
		tgbotapi.NewKeyboardButtonRow(contactBtn),
	)
	keyboard.OneTimeKeyboard = true
	msg.ReplyMarkup = keyboard
	bot.Send(msg)
	userStates[chatID] = UserState{
		State:           stateWaitingForPhone,
		Name:            userStates[chatID].Name,
		PhoneContact:    userStates[chatID].PhoneContact,
		PhoneManual:     userStates[chatID].PhoneManual,
		Guests:          userStates[chatID].Guests,
		Date:            userStates[chatID].Date,
		Comment:         userStates[chatID].Comment,
		TempReservation: userStates[chatID].TempReservation,
	}
}

func processDateSelection(bot *tgbotapi.BotAPI, chatID int64, selectedDate string) {
	state := userStates[chatID]
	state.Date = selectedDate
	state.State = stateWaitingForTime
	userStates[chatID] = state
	askForTime(bot, chatID)
}

func processTimeSelection(bot *tgbotapi.BotAPI, chatID int64, selectedTime string) {
	state := userStates[chatID]

	phone := state.PhoneContact
	if phone == "" {
		phone = state.PhoneManual
	}

	currentTime := time.Now().In(loc)
	reservation := Reservation{
		ID:        fmt.Sprintf("%d-%d", chatID, currentTime.UnixNano()),
		ChatID:    chatID,
		Name:      state.Name,
		Phone:     phone,
		Guests:    state.Guests,
		Date:      state.Date,
		Time:      selectedTime,
		Comment:   state.Comment,
		Confirmed: true,
		CreatedAt: currentTime,
	}

	log.Printf("Создана новая бронь: ID=%s, Имя='%s', Телефон='%s'", reservation.ID, reservation.Name, reservation.Phone)

	reservations[reservation.ID] = reservation
	saveReservationToFile(reservation)

	// Очищаем состояние пользователя после создания брони
	clearUserState(chatID)

	if adminChatID != 0 {
		adminMsg := tgbotapi.NewMessage(adminChatID, fmt.Sprintf(
			"Новая бронь #%s!\nИмя: %s\nТелефон: %s\nГостей: %d\nДата: %s\nВремя: %s\nКомментарий: %s",
			reservation.ID, reservation.Name, reservation.Phone, reservation.Guests,
			reservation.Date, reservation.Time, reservation.Comment))
		bot.Send(adminMsg)
	}

	confirmationMsg := fmt.Sprintf(
		"✅ Бронь #%s успешна!\n\nДетали:\nИмя: %s\nТелефон: %s\nГостей: %d\nДата: %s\nВремя: %s",
		reservation.ID, reservation.Name, reservation.Phone, reservation.Guests, reservation.Date, reservation.Time)

	if reservation.Comment != "" && reservation.Comment != "-" {
		confirmationMsg += fmt.Sprintf("\nКомментарий: %s", reservation.Comment)
	}

	msg := tgbotapi.NewMessage(chatID, confirmationMsg)
	msg.ReplyMarkup = tgbotapi.NewReplyKeyboard(
		tgbotapi.NewKeyboardButtonRow(
			tgbotapi.NewKeyboardButton("Моя бронь"),
			tgbotapi.NewKeyboardButton("Забронировать стол"),
		),
		tgbotapi.NewKeyboardButtonRow(
			tgbotapi.NewKeyboardButton("Связаться с нами"),
		),
	)
	bot.Send(msg)
}

func handleEditAction(bot *tgbotapi.BotAPI, chatID int64, action string) {
	if strings.HasPrefix(action, "select_") {
		reservationID := strings.TrimPrefix(action, "select_")
		if reservation, exists := reservations[reservationID]; exists {
			userStates[chatID] = UserState{
				State:           stateEditingReservation,
				Name:            reservation.Name,
				PhoneContact:    reservation.Phone,
				PhoneManual:     reservation.Phone,
				Guests:          reservation.Guests,
				Date:            reservation.Date,
				Comment:         reservation.Comment,
				TempReservation: &reservation,
			}
			showEditOptions(bot, chatID, reservation)
		}
	} else if strings.HasPrefix(action, "delete_") {
		reservationID := strings.TrimPrefix(action, "delete_")
		if reservation, exists := reservations[reservationID]; exists {
			delete(reservations, reservationID)
			deleteReservationFromFile(reservationID)

			if adminChatID != 0 {
				adminMsg := tgbotapi.NewMessage(adminChatID, fmt.Sprintf(
					"❌ Бронь #%s удалена!\nИмя: %s\nТелефон: %s\nГостей: %d\nДата: %s\nВремя: %s",
					reservation.ID, reservation.Name, reservation.Phone, reservation.Guests,
					reservation.Date, reservation.Time))
				bot.Send(adminMsg)
			}

			sendMessage(bot, chatID, fmt.Sprintf("Бронь #%s успешно удалена", reservationID), false)
			clearUserState(chatID)
			showMainMenu(bot, chatID, hasActiveReservations(chatID))
		}
	} else {
		state := userStates[chatID]
		if state.TempReservation == nil {
			sendMessage(bot, chatID, "Ошибка редактирования. Пожалуйста, начните заново.", false)
			clearUserState(chatID)
			showMainMenu(bot, chatID, hasActiveReservations(chatID))
			return
		}

		currentReservation := *state.TempReservation

		switch action {
		case "change_name":
			userStates[chatID] = UserState{
				State:           stateEditingReservationName,
				Name:            state.Name,
				PhoneContact:    state.PhoneContact,
				PhoneManual:     state.PhoneManual,
				Guests:          state.Guests,
				Date:            state.Date,
				Comment:         state.Comment,
				TempReservation: state.TempReservation,
			}
			sendMessage(bot, chatID, fmt.Sprintf("Текущее имя: %s. Введите новое имя:", currentReservation.Name), true)
			return
		case "change_phone":
			userStates[chatID] = UserState{
				State:           stateEditingReservationPhone,
				Name:            state.Name,
				PhoneContact:    state.PhoneContact,
				PhoneManual:     state.PhoneManual,
				Guests:          state.Guests,
				Date:            state.Date,
				Comment:         state.Comment,
				TempReservation: state.TempReservation,
			}
			sendMessage(bot, chatID, fmt.Sprintf("Текущий телефон: %s. Введите новый телефон:", currentReservation.Phone), true)
			return
		case "change_guests":
			userStates[chatID] = UserState{
				State:           stateEditingReservationGuests,
				Name:            state.Name,
				PhoneContact:    state.PhoneContact,
				PhoneManual:     state.PhoneManual,
				Guests:          state.Guests,
				Date:            state.Date,
				Comment:         state.Comment,
				TempReservation: state.TempReservation,
			}
			sendMessage(bot, chatID, fmt.Sprintf("Текущее количество гостей: %d. Введите новое количество:", currentReservation.Guests), true)
			return
		case "change_date":
			userStates[chatID] = UserState{
				State:           stateEditingReservationDate,
				Name:            state.Name,
				PhoneContact:    state.PhoneContact,
				PhoneManual:     state.PhoneManual,
				Guests:          state.Guests,
				Date:            state.Date,
				Comment:         state.Comment,
				TempReservation: state.TempReservation,
			}
			askForDate(bot, chatID)
			return
		case "change_time":
			userStates[chatID] = UserState{
				State:           stateEditingReservationTime,
				Name:            state.Name,
				PhoneContact:    state.PhoneContact,
				PhoneManual:     state.PhoneManual,
				Guests:          state.Guests,
				Date:            state.Date,
				Comment:         state.Comment,
				TempReservation: state.TempReservation,
			}
			askForTime(bot, chatID)
			return
		case "change_comment":
			userStates[chatID] = UserState{
				State:           stateEditingReservationComment,
				Name:            state.Name,
				PhoneContact:    state.PhoneContact,
				PhoneManual:     state.PhoneManual,
				Guests:          state.Guests,
				Date:            state.Date,
				Comment:         state.Comment,
				TempReservation: state.TempReservation,
			}
			sendMessage(bot, chatID, fmt.Sprintf("Текущий комментарий: %s. Введите новый комментарий:", currentReservation.Comment), true)
			return
		case "confirm":
			// Сохраняем обновленную бронь
			reservations[currentReservation.ID] = currentReservation
			updateReservationInFile(currentReservation)

			// Очищаем состояние пользователя после редактирования
			clearUserState(chatID)

			if adminChatID != 0 {
				adminMsg := tgbotapi.NewMessage(adminChatID, fmt.Sprintf(
					"✏️ Бронь #%s отредактирована!\nИмя: %s\nТелефон: %s\nГостей: %d\nДата: %s\nВремя: %s\nКомментарий: %s",
					currentReservation.ID, currentReservation.Name, currentReservation.Phone, currentReservation.Guests,
					currentReservation.Date, currentReservation.Time, currentReservation.Comment))
				bot.Send(adminMsg)
			}

			sendMessage(bot, chatID, "✅ Изменения сохранены!", false)
			showMainMenu(bot, chatID, true)
		}
	}
}

func showEditOptions(bot *tgbotapi.BotAPI, chatID int64, reservation Reservation) {
	msg := tgbotapi.NewMessage(chatID, fmt.Sprintf(
		"Редактирование брони #%s:\n\nИмя: %s\nТелефон: %s\nГостей: %d\nДата: %s\nВремя: %s\nКомментарий: %s\n\nЧто хотите изменить?",
		reservation.ID, reservation.Name, reservation.Phone, reservation.Guests, reservation.Date, reservation.Time, reservation.Comment))

	buttons := [][]tgbotapi.InlineKeyboardButton{
		{tgbotapi.NewInlineKeyboardButtonData("Изменить имя", "edit_change_name")},
		{tgbotapi.NewInlineKeyboardButtonData("Изменить телефон", "edit_change_phone")},
		{tgbotapi.NewInlineKeyboardButtonData("Изменить количество гостей", "edit_change_guests")},
		{tgbotapi.NewInlineKeyboardButtonData("Изменить дату", "edit_change_date")},
		{tgbotapi.NewInlineKeyboardButtonData("Изменить время", "edit_change_time")},
		{tgbotapi.NewInlineKeyboardButtonData("Изменить комментарий", "edit_change_comment")},
		{tgbotapi.NewInlineKeyboardButtonData("✅ Подтвердить изменения", "edit_confirm")},
		{tgbotapi.NewInlineKeyboardButtonData("❌ Отмена", "cancel")},
	}

	msg.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(buttons...)
	bot.Send(msg)
}

func sendMessage(bot *tgbotapi.BotAPI, chatID int64, text string, hideKeyboard bool) {
	msg := tgbotapi.NewMessage(chatID, text)
	if hideKeyboard {
		msg.ReplyMarkup = tgbotapi.NewRemoveKeyboard(true)
	}
	bot.Send(msg)
}

func saveReservationToFile(reservation Reservation) {
	file, err := os.OpenFile(reservationsFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		log.Printf("Ошибка при открытии файла для записи: %v", err)
		return
	}
	defer file.Close()

	writer := csv.NewWriter(file)
	record := []string{
		reservation.ID,
		strconv.FormatInt(reservation.ChatID, 10),
		reservation.Name,
		reservation.Phone,
		strconv.Itoa(reservation.Guests),
		reservation.Date,
		reservation.Time,
		reservation.Comment,
		strconv.FormatBool(reservation.Confirmed),
		reservation.CreatedAt.Format(time.RFC3339),
	}

	if err := writer.Write(record); err != nil {
		log.Printf("Ошибка записи брони в файл: %v", err)
	}
	writer.Flush()

	if err := writer.Error(); err != nil {
		log.Printf("Ошибка при сохранении файла: %v", err)
	}

	log.Printf("Бронь сохранена в файл: ID=%s, Имя='%s'", reservation.ID, reservation.Name)
}

func updateReservationInFile(reservation Reservation) {
	file, err := os.OpenFile(reservationsFile, os.O_RDWR|os.O_CREATE, 0644)
	if err != nil {
		log.Printf("Ошибка при открытии файла для обновления: %v", err)
		return
	}
	defer file.Close()

	reader := csv.NewReader(file)
	reader.Comma = ','
	reader.FieldsPerRecord = -1

	if _, err := reader.Read(); err != nil {
		log.Printf("Ошибка чтения заголовка: %v", err)
		return
	}

	records, err := reader.ReadAll()
	if err != nil {
		log.Printf("Ошибка чтения файла для обновления: %v", err)
		return
	}

	file.Truncate(0)
	file.Seek(0, 0)
	writer := csv.NewWriter(file)

	headers := []string{
		"ID",
		"ChatID",
		"Name",
		"Phone",
		"Guests",
		"Date",
		"Time",
		"Comment",
		"Confirmed",
		"CreatedAt",
	}
	writer.Write(headers)

	for _, record := range records {
		if len(record) > 0 && record[0] == reservation.ID {
			record = []string{
				reservation.ID,
				strconv.FormatInt(reservation.ChatID, 10),
				reservation.Name,
				reservation.Phone,
				strconv.Itoa(reservation.Guests),
				reservation.Date,
				reservation.Time,
				reservation.Comment,
				strconv.FormatBool(reservation.Confirmed),
				reservation.CreatedAt.Format(time.RFC3339),
			}
		}
		if len(record) > 0 {
			writer.Write(record)
		}
	}
	writer.Flush()

	if err := writer.Error(); err != nil {
		log.Printf("Ошибка при сохранении файла после обновления: %v", err)
	}

	log.Printf("Бронь обновлена в файле: ID=%s, Имя='%s'", reservation.ID, reservation.Name)
}

func deleteReservationFromFile(id string) {
	file, err := os.OpenFile(reservationsFile, os.O_RDWR|os.O_CREATE, 0644)
	if err != nil {
		log.Printf("Ошибка при открытии файла для удаления: %v", err)
		return
	}
	defer file.Close()

	reader := csv.NewReader(file)
	reader.Comma = ','
	reader.FieldsPerRecord = -1

	if _, err := reader.Read(); err != nil {
		log.Printf("Ошибка чтения заголовка: %v", err)
		return
	}

	records, err := reader.ReadAll()
	if err != nil {
		log.Printf("Ошибка чтения файла для удаления: %v", err)
		return
	}

	file.Truncate(0)
	file.Seek(0, 0)
	writer := csv.NewWriter(file)

	headers := []string{
		"ID",
		"ChatID",
		"Name",
		"Phone",
		"Guests",
		"Date",
		"Time",
		"Comment",
		"Confirmed",
		"CreatedAt",
	}
	writer.Write(headers)

	for _, record := range records {
		if len(record) > 0 && record[0] != id {
			writer.Write(record)
		}
	}
	writer.Flush()

	if err := writer.Error(); err != nil {
		log.Printf("Ошибка при сохранении файла после удаления: %v", err)
	}
}
