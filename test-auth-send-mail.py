#!/usr/bin/env python3

import smtplib, ssl
from email.mime.text import MIMEText

smtp_server = "<ip-of-mail-server>"
smtp_port = 11587
smtp_auth_user = "jmail"
smtp_auth_password = "mysecretpassword"

sender = "noreply@<your-domain>"
receiver = "<your-email-address>"
email_body = "This is a test email from Jmail."

# Try to log in to server and send email
try:
    msg = MIMEText(email_body)
    msg["Subject"] = "Test email from Jmail!"
    msg["From"] = sender
    msg["To"] = receiver

    server = smtplib.SMTP(smtp_server, smtp_port)
    server.ehlo()
    server.login(smtp_auth_user, smtp_auth_password)
    server.send_email(sender, receiver, msg.as_string())
except Exception as e:
    # Print any error messages to stdout
    print(e)
finally:
    server.quit()
